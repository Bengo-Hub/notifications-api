// Package announcements serves the dashboard banners: the platform publishes to every tenant, a
// tenant publishes to its own users, and every app's dashboard shows the active ones for its
// service.
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
	"github.com/bengobox/notifications-api/internal/ent/predicate"
	"github.com/bengobox/notifications-api/internal/ent/tenant"
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
// every app), from the platform or from the viewer's own tenant (nil = platform banners only),
// highest priority first, then newest.
func ActiveFor(list []*ent.Announcement, service string, tenantID *uuid.UUID, now time.Time) []*ent.Announcement {
	service = strings.ToLower(strings.TrimSpace(service))
	out := make([]*ent.Announcement, 0, len(list))
	for _, a := range list {
		if !a.IsActive || a.StartsAt.After(now) || (a.EndsAt != nil && !a.EndsAt.After(now)) {
			continue
		}
		if a.TenantID != nil && (tenantID == nil || *a.TenantID != *tenantID) {
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
	slugs    map[string]uuid.UUID // public ?tenant= slug lookups
}

// NewService builds the service.
func NewService(client *ent.Client) *Service {
	return &Service{client: client}
}

// Active returns what the service's app shows now for a viewer's tenant (nil = platform banners
// only). The running set (live banners only, so it stays small) is loaded once per cacheTTL per
// pod, so dashboards polling it never reach the database per request.
func (s *Service) Active(ctx context.Context, service string, tenantID *uuid.UUID) ([]*ent.Announcement, error) {
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
	return ActiveFor(s.cached, service, tenantID, now), nil
}

// ResolveTenant turns a public ?tenant= value (UUID or slug) into the tenant's id; nil when it is
// empty or unknown, which shows platform banners only.
func (s *Service) ResolveTenant(ctx context.Context, ref string) *uuid.UUID {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	if id, err := uuid.Parse(ref); err == nil {
		return &id
	}
	s.mu.Lock()
	if id, ok := s.slugs[ref]; ok {
		s.mu.Unlock()
		return &id
	}
	s.mu.Unlock()
	t, err := s.client.Tenant.Query().Where(tenant.Slug(ref)).Only(ctx)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	if s.slugs == nil {
		s.slugs = map[string]uuid.UUID{}
	}
	s.slugs[ref] = t.ID
	s.mu.Unlock()
	return &t.ID
}

func (s *Service) invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

// owned scopes a query to one owner: platform banners (nil) or one tenant's.
func owned(tenantID *uuid.UUID) predicate.Announcement {
	if tenantID == nil {
		return announcement.TenantIDIsNil()
	}
	return announcement.TenantID(*tenantID)
}

// List returns an owner's announcements, newest first.
func (s *Service) List(ctx context.Context, tenantID *uuid.UUID) ([]*ent.Announcement, error) {
	return s.client.Announcement.Query().Where(owned(tenantID)).Order(ent.Desc(announcement.FieldCreatedAt)).Limit(500).All(ctx)
}

// Create publishes a new announcement for an owner (nil = platform).
func (s *Service) Create(ctx context.Context, in Input, createdBy string, tenantID *uuid.UUID) (*ent.Announcement, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	c := s.client.Announcement.Create().SetNillableTenantID(tenantID).
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

// Update replaces an announcement's content and schedule (only the owner's own rows).
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input, tenantID *uuid.UUID) (*ent.Announcement, error) {
	if err := in.Normalize(); err != nil {
		return nil, err
	}
	if _, err := s.client.Announcement.Query().Where(announcement.ID(id), owned(tenantID)).Only(ctx); err != nil {
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

// Delete removes an owner's announcement.
func (s *Service) Delete(ctx context.Context, id uuid.UUID, tenantID *uuid.UUID) error {
	n, err := s.client.Announcement.Delete().Where(announcement.ID(id), owned(tenantID)).Exec(ctx)
	if err == nil && n == 0 {
		return &ent.NotFoundError{}
	}
	if err == nil {
		s.invalidate()
	}
	return err
}
