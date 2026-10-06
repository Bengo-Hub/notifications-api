// Package announcements serves the platform's "what's new" banners: the platform admin publishes
// them, every app's dashboard shows the active ones for its service.
package announcements

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/announcement"
)

// cacheTTL bounds how stale the public read is on a pod other than the one that took a write.
const cacheTTL = 30 * time.Second

// ErrInvalid wraps every validation failure (a 400 for the caller).
var ErrInvalid = errors.New("invalid announcement")

// Input is a create or full update.
type Input struct {
	Title       string         `json:"title"`
	Summary     string         `json:"summary"`
	Highlights  []string       `json:"highlights"`
	CTALabel    string         `json:"cta_label"`
	CTAURL      string         `json:"cta_url"`
	Services    []string       `json:"services"`
	Audience    string         `json:"audience"`
	Tone        string         `json:"tone"`
	Priority    int            `json:"priority"`
	Dismissible *bool          `json:"dismissible"`
	IsActive    *bool          `json:"is_active"`
	StartsAt    *time.Time     `json:"starts_at"`
	EndsAt      *time.Time     `json:"ends_at"`
	Metadata    map[string]any `json:"metadata"`
}

// Normalize trims and validates the input in place.
func (in *Input) Normalize() error {
	in.Title = strings.TrimSpace(in.Title)
	in.Summary = strings.TrimSpace(in.Summary)
	in.CTALabel = strings.TrimSpace(in.CTALabel)
	in.CTAURL = strings.TrimSpace(in.CTAURL)
	if in.Title == "" || len(in.Title) > 120 {
		return fmt.Errorf("%w: title is required (at most 120 characters)", ErrInvalid)
	}
	if in.Summary == "" {
		return fmt.Errorf("%w: summary is required", ErrInvalid)
	}
	highlights := in.Highlights[:0]
	for _, h := range in.Highlights {
		if h = strings.TrimSpace(h); h != "" {
			highlights = append(highlights, h)
		}
	}
	in.Highlights = highlights
	services := make([]string, 0, len(in.Services))
	for _, s := range in.Services {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" && !slices.Contains(services, s) {
			services = append(services, s)
		}
	}
	in.Services = services
	if (in.CTALabel == "") != (in.CTAURL == "") {
		return fmt.Errorf("%w: cta_label and cta_url go together", ErrInvalid)
	}
	if in.CTAURL != "" && !safeLink(in.CTAURL) {
		return fmt.Errorf("%w: cta_url must be an https URL, a mailto address or an app path starting with /", ErrInvalid)
	}
	if in.Audience == "" {
		in.Audience = string(announcement.AudienceAll)
	}
	if announcement.AudienceValidator(announcement.Audience(in.Audience)) != nil {
		return fmt.Errorf("%w: audience must be all or admins", ErrInvalid)
	}
	if in.Tone == "" {
		in.Tone = string(announcement.ToneFeature)
	}
	if announcement.ToneValidator(announcement.Tone(in.Tone)) != nil {
		return fmt.Errorf("%w: tone must be feature, info or warning", ErrInvalid)
	}
	if in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt) {
		return fmt.Errorf("%w: ends_at must be after starts_at", ErrInvalid)
	}
	return normalizeVariants(in.Metadata)
}

// safeLink allows https links, mailto addresses and same-app paths only, so a banner can never
// carry a javascript: or plain-http link.
func safeLink(raw string) bool {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		return true
	}
	u, err := url.Parse(strings.ReplaceAll(raw, "{orgSlug}", "org"))
	if err != nil {
		return false
	}
	if u.Scheme == "mailto" {
		return strings.Contains(u.Opaque, "@")
	}
	return u.Scheme == "https" && u.Host != ""
}

// ActiveFor picks what an app shows now: active, started, not ended, aimed at the service (or
// every app), highest priority first, then newest.
func ActiveFor(list []*ent.Announcement, service string, now time.Time) []*ent.Announcement {
	service = strings.ToLower(strings.TrimSpace(service))
	out := make([]*ent.Announcement, 0, len(list))
	for _, a := range list {
		if !a.IsActive || a.StartsAt.After(now) || (a.EndsAt != nil && !a.EndsAt.After(now)) {
			continue
		}
		if len(a.Services) > 0 && !slices.Contains(a.Services, service) {
			continue
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].StartsAt.After(out[j].StartsAt)
	})
	return out
}

// Service reads and writes announcements.
type Service struct {
	client *ent.Client

	mu       sync.Mutex
	cached   []*ent.Announcement
	cachedAt time.Time
}

// NewService builds the service.
func NewService(client *ent.Client) *Service {
	return &Service{client: client}
}

// Active returns what the service's app shows now. The running set is loaded once per cacheTTL
// per pod, so dashboards polling it never reach the database per request.
func (s *Service) Active(ctx context.Context, service string) ([]*ent.Announcement, error) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == nil || now.Sub(s.cachedAt) > cacheTTL {
		list, err := s.client.Announcement.Query().
			Where(
				announcement.IsActive(true),
				announcement.StartsAtLTE(now),
				announcement.Or(announcement.EndsAtIsNil(), announcement.EndsAtGT(now)),
			).
			All(ctx)
		if err != nil {
			return nil, err
		}
		s.cached, s.cachedAt = list, now
	}
	return ActiveFor(s.cached, service, now), nil
}

func (s *Service) invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

// List returns every announcement, newest first, for the platform admin.
func (s *Service) List(ctx context.Context) ([]*ent.Announcement, error) {
	return s.client.Announcement.Query().Order(ent.Desc(announcement.FieldCreatedAt)).Limit(500).All(ctx)
}

// Create publishes a new announcement.
func (s *Service) Create(ctx context.Context, in Input, createdBy string) (*ent.Announcement, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	c := s.client.Announcement.Create().
		SetTitle(in.Title).SetSummary(in.Summary).SetHighlights(in.Highlights).
		SetServices(in.Services).SetAudience(announcement.Audience(in.Audience)).
		SetTone(announcement.Tone(in.Tone)).SetPriority(in.Priority).
		SetNillableEndsAt(in.EndsAt).SetCreatedBy(createdBy)
	if in.CTAURL != "" {
		c.SetCtaLabel(in.CTALabel).SetCtaURL(in.CTAURL)
	}
	if in.Dismissible != nil {
		c.SetDismissible(*in.Dismissible)
	}
	if in.IsActive != nil {
		c.SetIsActive(*in.IsActive)
	}
	if in.StartsAt != nil {
		c.SetStartsAt(*in.StartsAt)
	}
	if in.Metadata != nil {
		c.SetMetadata(in.Metadata)
	}
	a, err := c.Save(ctx)
	if err == nil {
		s.invalidate()
	}
	return a, err
}

// Update replaces an announcement's content and schedule.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (*ent.Announcement, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	u := s.client.Announcement.UpdateOneID(id).
		SetTitle(in.Title).SetSummary(in.Summary).SetHighlights(in.Highlights).
		SetServices(in.Services).SetAudience(announcement.Audience(in.Audience)).
		SetTone(announcement.Tone(in.Tone)).SetPriority(in.Priority)
	if in.CTAURL != "" {
		u.SetCtaLabel(in.CTALabel).SetCtaURL(in.CTAURL)
	} else {
		u.ClearCtaLabel().ClearCtaURL()
	}
	if in.EndsAt != nil {
		u.SetEndsAt(*in.EndsAt)
	} else {
		u.ClearEndsAt()
	}
	if in.Dismissible != nil {
		u.SetDismissible(*in.Dismissible)
	}
	if in.IsActive != nil {
		u.SetIsActive(*in.IsActive)
	}
	if in.StartsAt != nil {
		u.SetStartsAt(*in.StartsAt)
	}
	if in.Metadata != nil {
		u.SetMetadata(in.Metadata)
	}
	a, err := u.Save(ctx)
	if err == nil {
		s.invalidate()
	}
	return a, err
}

// Delete removes an announcement.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	err := s.client.Announcement.DeleteOneID(id).Exec(ctx)
	if err == nil {
		s.invalidate()
	}
	return err
}
