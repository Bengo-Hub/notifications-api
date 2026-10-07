package occasions

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	entoccasion "github.com/bengobox/notifications-api/internal/ent/occasion"
)

//go:embed catalog.json
var catalogJSON []byte

// Variant is one wording of an occasion message. Email uses subject and body, SMS uses sms.
type Variant struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	SMS     string `json:"sms"`
}

// Settings are the sender's choices for an occasion, kept in Occasion.metadata.
type Settings struct {
	Enabled          bool      `json:"enabled"`
	AutoSend         bool      `json:"auto_send"`
	Channels         []string  `json:"channels"`
	Audience         string    `json:"audience,omitempty"`  // tenant_customers (default for tenants) or tenant_users
	SegmentID        string    `json:"segment_id,omitempty"` // MarketFlow segment for tenant_customers
	SendTime         string    `json:"send_time,omitempty"`  // HH:MM local, default 09:00
	WhatsAppTemplate string    `json:"whatsapp_template,omitempty"`
	Variants         []Variant `json:"variants"`
}

// View is an occasion as a sender sees it: the catalogue row merged with their own settings.
type View struct {
	ID             uuid.UUID `json:"id"`
	Key            string    `json:"key"`
	Name           string    `json:"name"`
	Country        string    `json:"country"`
	Rule           Rule      `json:"rule"`
	DurationDays   int       `json:"duration_days"`
	LeadDays       int       `json:"lead_days"`
	SendOffsetDays int       `json:"send_offset_days"`
	Custom         bool      `json:"custom"` // the sender's own occasion, not from the catalogue
	Customised     bool      `json:"customised"`
	Settings       Settings  `json:"settings"`
	NextDate       string    `json:"next_date,omitempty"` // YYYY-MM-DD, empty when no date is set
	NeedsDate      bool      `json:"needs_date"`          // explicit rule with no date for the coming year
	TenantRowID    uuid.UUID `json:"-"`
	CatalogRowID   uuid.UUID `json:"-"`
}

// SendDate is the day the greeting goes out for the occurrence starting on start.
func (v View) SendDate(start time.Time) time.Time {
	return start.AddDate(0, 0, v.SendOffsetDays)
}

type catalogEntry struct {
	Key              string         `json:"key"`
	Name             string         `json:"name"`
	Country          string         `json:"country"`
	Rule             map[string]any `json:"rule"`
	DurationDays     int            `json:"duration_days"`
	LeadDays         int            `json:"lead_days"`
	SendOffsetDays   int            `json:"send_offset_days"`
	Enabled          bool           `json:"enabled"`
	Channels         []string       `json:"channels"`
	WhatsAppTemplate string         `json:"whatsapp_template"`
	Variants         []Variant      `json:"variants"`
}

// Service reads and writes occasions.
type Service struct {
	client *ent.Client
	log    *zap.Logger
}

// NewService builds the service.
func NewService(client *ent.Client, log *zap.Logger) *Service {
	return &Service{client: client, log: log.Named("occasions")}
}

// Seed inserts catalogue occasions that do not exist yet. Existing rows are never changed, so a
// platform admin's edits (wording, dates, switched off) survive every deploy.
func (s *Service) Seed(ctx context.Context) error {
	var entries []catalogEntry
	if err := json.Unmarshal(catalogJSON, &entries); err != nil {
		return fmt.Errorf("parse occasion catalog: %w", err)
	}
	existing, err := s.client.Occasion.Query().Where(entoccasion.TenantIDIsNil()).Select(entoccasion.FieldKey).Strings(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, k := range existing {
		have[k] = true
	}
	for _, e := range entries {
		if have[e.Key] {
			continue
		}
		if err := RuleFromMap(e.Rule).Validate(); err != nil {
			return fmt.Errorf("catalog %s: %w", e.Key, err)
		}
		meta := settingsToMap(Settings{Enabled: e.Enabled, Channels: e.Channels, WhatsAppTemplate: e.WhatsAppTemplate, Variants: e.Variants})
		c := s.client.Occasion.Create().
			SetKey(e.Key).SetName(e.Name).SetCountry(e.Country).SetRule(e.Rule).
			SetLeadDays(orDefault(e.LeadDays, 14)).SetDurationDays(orDefault(e.DurationDays, 1)).
			SetSendOffsetDays(e.SendOffsetDays).SetMetadata(meta)
		if err := c.Exec(ctx); err != nil && !ent.IsConstraintError(err) {
			return fmt.Errorf("seed occasion %s: %w", e.Key, err)
		}
	}
	return nil
}

// List returns the occasions a sender sees, with each one's next date: the platform catalogue
// for the platform (tenantID nil); for a tenant, the catalogue merged with the tenant's own rows
// plus the tenant's custom occasions. Tenants see catalogue occasions switched off until they
// switch them on (their customers, their choice).
func (s *Service) List(ctx context.Context, tenantID *uuid.UUID, now time.Time, loc *time.Location) ([]View, error) {
	catalog, err := s.client.Occasion.Query().Where(entoccasion.TenantIDIsNil(), entoccasion.IsActive(true)).All(ctx)
	if err != nil {
		return nil, err
	}
	var own []*ent.Occasion
	if tenantID != nil {
		own, err = s.client.Occasion.Query().Where(entoccasion.TenantID(*tenantID)).All(ctx)
		if err != nil {
			return nil, err
		}
	}
	byKey := map[string]*ent.Occasion{}
	for _, o := range own {
		byKey[o.Key] = o
	}
	var out []View
	for _, c := range catalog {
		v := toView(c)
		v.CatalogRowID = c.ID
		if tenantID != nil {
			v.Settings.Enabled = false
			v.Settings.AutoSend = false
			if o, ok := byKey[c.Key]; ok {
				v.Settings = mergeSettings(v.Settings, settingsFromMap(o.Metadata))
				v.Customised = true
				v.TenantRowID = o.ID
				delete(byKey, c.Key)
			}
		}
		out = append(out, withNextDate(v, now, loc))
	}
	for _, o := range byKey { // the tenant's own occasions
		v := toView(o)
		v.Custom = true
		v.TenantRowID = o.ID
		out = append(out, withNextDate(v, now, loc))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NextDate == out[j].NextDate {
			return out[i].Name < out[j].Name
		}
		if out[i].NextDate == "" {
			return false
		}
		if out[j].NextDate == "" {
			return true
		}
		return out[i].NextDate < out[j].NextDate
	})
	return out, nil
}

// Get returns one occasion by key as the sender sees it.
func (s *Service) Get(ctx context.Context, tenantID *uuid.UUID, key string, now time.Time, loc *time.Location) (View, error) {
	list, err := s.List(ctx, tenantID, now, loc)
	if err != nil {
		return View{}, err
	}
	for _, v := range list {
		if v.Key == key {
			return v, nil
		}
	}
	return View{}, ErrNotFound
}

// ErrNotFound is returned for an unknown occasion key.
var ErrNotFound = errors.New("occasion not found")

// Update is a sender's change to an occasion. For the platform it edits the catalogue row; for a
// tenant it writes the tenant's own row (customising a catalogue occasion, or a custom one).
type Update struct {
	Name           *string
	Rule           map[string]any
	LeadDays       *int
	SendOffsetDays *int
	Settings       *Settings
}

// Save applies an update. A tenant may only change dates on its own custom occasions; catalogue
// dates are shared and owned by the platform.
func (s *Service) Save(ctx context.Context, tenantID *uuid.UUID, key string, u Update) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("key is required")
	}
	if u.Rule != nil {
		if err := RuleFromMap(u.Rule).Validate(); err != nil {
			return err
		}
	}
	if u.Settings != nil {
		if err := validateSettings(*u.Settings); err != nil {
			return err
		}
	}

	if tenantID == nil {
		row, err := s.client.Occasion.Query().Where(entoccasion.TenantIDIsNil(), entoccasion.Key(key)).Only(ctx)
		if ent.IsNotFound(err) {
			if u.Name == nil || u.Rule == nil {
				return ErrNotFound
			}
			c := s.client.Occasion.Create().SetKey(key).SetName(*u.Name).SetRule(u.Rule).SetCountry("")
			applyCreate(c, u)
			return c.Exec(ctx)
		}
		if err != nil {
			return err
		}
		return applyUpdate(row.Update(), row.Metadata, u).Exec(ctx)
	}

	catalogRow, cerr := s.client.Occasion.Query().Where(entoccasion.TenantIDIsNil(), entoccasion.Key(key)).Only(ctx)
	if cerr != nil && !ent.IsNotFound(cerr) {
		return cerr
	}
	fromCatalog := cerr == nil
	if fromCatalog && (u.Rule != nil || u.LeadDays != nil || u.SendOffsetDays != nil) {
		return errors.New("dates of shared occasions are set by the platform; add your own occasion to use another date")
	}
	row, err := s.client.Occasion.Query().Where(entoccasion.TenantID(*tenantID), entoccasion.Key(key)).Only(ctx)
	if ent.IsNotFound(err) {
		c := s.client.Occasion.Create().SetTenantID(*tenantID).SetKey(key)
		switch {
		case fromCatalog:
			c.SetName(catalogRow.Name).SetRule(catalogRow.Rule).SetCountry(catalogRow.Country).
				SetLeadDays(catalogRow.LeadDays).SetSendOffsetDays(catalogRow.SendOffsetDays).SetDurationDays(catalogRow.DurationDays)
		case u.Name != nil && u.Rule != nil:
			c.SetName(*u.Name).SetRule(u.Rule).SetCountry("")
		default:
			return errors.New("a new occasion needs a name and a date rule")
		}
		applyCreate(c, u)
		return c.Exec(ctx)
	}
	if err != nil {
		return err
	}
	return applyUpdate(row.Update(), row.Metadata, u).Exec(ctx)
}

// Delete removes a tenant's own row (a custom occasion, or its customisation of a catalogue one,
// which then reverts to the shared wording, switched off).
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, key string) error {
	_, err := s.client.Occasion.Delete().Where(entoccasion.TenantID(tenantID), entoccasion.Key(key)).Exec(ctx)
	return err
}

// PickVariant chooses the wording for a year: it rotates through the variants by year so the
// same people never get last year's message again (with two or more variants).
func PickVariant(variants []Variant, year int) (Variant, int) {
	if len(variants) == 0 {
		return Variant{}, -1
	}
	i := year % len(variants)
	return variants[i], i
}

// Due is an occasion whose draft should exist now: its send date falls within lead days.
type Due struct {
	View     View
	TenantID *uuid.UUID
	Start    time.Time // occurrence start date
	SendOn   time.Time // the day it is sent
	Year     int
}

// DueSoon lists the enabled occasions of every sender whose send date is within their lead days
// of now. Rows are read in two queries (catalogue, then every tenant row), never per tenant.
func (s *Service) DueSoon(ctx context.Context, now time.Time, loc *time.Location) ([]Due, error) {
	catalog, err := s.client.Occasion.Query().Where(entoccasion.TenantIDIsNil(), entoccasion.IsActive(true)).All(ctx)
	if err != nil {
		return nil, err
	}
	tenantRows, err := s.client.Occasion.Query().Where(entoccasion.TenantIDNotNil(), entoccasion.IsActive(true)).All(ctx)
	if err != nil {
		return nil, err
	}
	catalogByKey := map[string]*ent.Occasion{}
	var out []Due
	add := func(v View, tenantID *uuid.UUID) {
		if !v.Settings.Enabled {
			return
		}
		start, err := v.Rule.NextOccurrence(now.AddDate(0, 0, -v.SendOffsetDays), loc)
		if err != nil {
			return
		}
		sendOn := v.SendDate(start)
		today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
		if sendOn.Before(today) || sendOn.After(today.AddDate(0, 0, v.LeadDays)) {
			return
		}
		out = append(out, Due{View: v, TenantID: tenantID, Start: start, SendOn: sendOn, Year: start.Year()})
	}
	for _, c := range catalog {
		catalogByKey[c.Key] = c
		add(toView(c), nil)
	}
	for _, o := range tenantRows {
		v := toView(o)
		if c, ok := catalogByKey[o.Key]; ok {
			base := toView(c)
			base.Settings = mergeSettings(Settings{Variants: base.Settings.Variants, Channels: base.Settings.Channels, WhatsAppTemplate: base.Settings.WhatsAppTemplate}, v.Settings)
			// Keep the catalogue id: a tenant's customised occasion is the same occasion, and the
			// one-broadcast-per-occasion-year guard keys on it. Using the tenant row's id here (as
			// List and DraftNow do not) let the planner and "Prepare this year's" make two drafts.
			v = base
		}
		tid := *o.TenantID
		add(v, &tid)
	}
	return out, nil
}

func withNextDate(v View, now time.Time, loc *time.Location) View {
	start, err := v.Rule.NextOccurrence(now.AddDate(0, 0, -v.SendOffsetDays), loc)
	if err != nil {
		v.NeedsDate = v.Rule.Type == RuleExplicit
		return v
	}
	v.NextDate = start.Format("2006-01-02")
	return v
}

func toView(o *ent.Occasion) View {
	return View{
		ID: o.ID, Key: o.Key, Name: o.Name, Country: o.Country, Rule: RuleFromMap(o.Rule),
		DurationDays: o.DurationDays, LeadDays: o.LeadDays, SendOffsetDays: o.SendOffsetDays,
		Settings: settingsFromMap(o.Metadata),
	}
}

func mergeSettings(base, own Settings) Settings {
	out := base
	out.Enabled = own.Enabled
	out.AutoSend = own.AutoSend
	if len(own.Channels) > 0 {
		out.Channels = own.Channels
	}
	if own.Audience != "" {
		out.Audience = own.Audience
	}
	if own.SegmentID != "" {
		out.SegmentID = own.SegmentID
	}
	if own.SendTime != "" {
		out.SendTime = own.SendTime
	}
	if len(own.Variants) > 0 {
		out.Variants = own.Variants
	}
	return out
}

func settingsFromMap(m map[string]any) Settings {
	var s Settings
	if len(m) == 0 {
		return s
	}
	b, _ := json.Marshal(m)
	_ = json.Unmarshal(b, &s)
	return s
}

func settingsToMap(s Settings) map[string]any {
	b, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

var validChannels = map[string]bool{"email": true, "sms": true, "whatsapp": true, "push": true, "in_app": true}

func validateSettings(s Settings) error {
	for _, c := range s.Channels {
		if !validChannels[c] {
			return fmt.Errorf("unknown channel %q", c)
		}
	}
	if s.SendTime != "" {
		if _, err := time.Parse("15:04", s.SendTime); err != nil {
			return errors.New("send_time must be HH:MM")
		}
	}
	for i, v := range s.Variants {
		if strings.TrimSpace(v.Body) == "" {
			return fmt.Errorf("wording %d has no message", i+1)
		}
	}
	return nil
}

func applyCreate(c *ent.OccasionCreate, u Update) {
	if u.LeadDays != nil {
		c.SetLeadDays(*u.LeadDays)
	}
	if u.SendOffsetDays != nil {
		c.SetSendOffsetDays(*u.SendOffsetDays)
	}
	if u.Settings != nil {
		c.SetMetadata(settingsToMap(*u.Settings))
	}
}

func applyUpdate(up *ent.OccasionUpdateOne, current map[string]any, u Update) *ent.OccasionUpdateOne {
	if u.Name != nil {
		up.SetName(*u.Name)
	}
	if u.Rule != nil {
		up.SetRule(u.Rule)
	}
	if u.LeadDays != nil {
		up.SetLeadDays(*u.LeadDays)
	}
	if u.SendOffsetDays != nil {
		up.SetSendOffsetDays(*u.SendOffsetDays)
	}
	if u.Settings != nil {
		merged := map[string]any{}
		for k, v := range current {
			merged[k] = v
		}
		for k, v := range settingsToMap(*u.Settings) {
			merged[k] = v
		}
		up.SetMetadata(merged)
	}
	return up
}

func orDefault(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
