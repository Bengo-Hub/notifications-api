package broadcasts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	entbroadcast "github.com/bengobox/notifications-api/internal/ent/broadcast"
	entrecipient "github.com/bengobox/notifications-api/internal/ent/broadcastrecipient"
)

// Content is what a broadcast says on each channel.
type Content struct {
	Email    *EmailContent    `json:"email,omitempty"`
	SMS      *SMSContent      `json:"sms,omitempty"`
	WhatsApp *WhatsAppContent `json:"whatsapp,omitempty"`
	Push     *PushContent     `json:"push,omitempty"`
	InApp    *InAppContent    `json:"in_app,omitempty"`
}

// EmailContent is a subject and a plain-text body (blank lines separate paragraphs).
type EmailContent struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

// SMSContent is the SMS text. Marketing SMS get "Reply STOP to opt out" added.
type SMSContent struct {
	Body string `json:"body"`
}

// WhatsAppContent names an approved template and the tokens filling its {{1}}, {{2}}, ... in
// order (first_name, sender_name, year, occasion, or "message" for the broadcast's own text).
type WhatsAppContent struct {
	Template string   `json:"template"`
	Params   []string `json:"params"`
	Message  string   `json:"message,omitempty"`
}

// PushContent is a push title and body.
type PushContent struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// InAppContent is a dashboard banner (stored as an Announcement while the broadcast is live).
type InAppContent struct {
	Title    string   `json:"title"`
	Summary  string   `json:"summary"`
	CTALabel string   `json:"cta_label,omitempty"`
	CTAURL   string   `json:"cta_url,omitempty"`
	Services []string `json:"services,omitempty"`
	Audience string   `json:"audience,omitempty"` // all | admins
	Tone     string   `json:"tone,omitempty"`
	Days     int      `json:"days,omitempty"` // how long the banner shows, default 7
}

// whatsappParamTokens are the values a WhatsApp template parameter may take.
var whatsappParamTokens = map[string]bool{"first_name": true, "name": true, "business_name": true, "sender_name": true, "year": true, "occasion": true, "message": true}

// Channels.
var knownChannels = map[string]bool{"email": true, "sms": true, "whatsapp": true, "push": true, "in_app": true}

// Input creates or edits a broadcast.
type Input struct {
	Title    string         `json:"title"`
	Kind     string         `json:"kind"`
	Class    string         `json:"class"`
	Channels []string       `json:"channels"`
	Content  Content        `json:"content"`
	Audience map[string]any `json:"audience"`
	SendAt   *time.Time     `json:"send_at"`
	// Sender-facing settings kept in metadata.
	SenderName      string `json:"sender_name"`
	Timezone        string `json:"timezone"`
	ConsentAttested bool   `json:"consent_attested"`
}

// Sender identifies who is sending: the platform (TenantID nil) or a tenant.
type Sender struct {
	TenantID *uuid.UUID
	Name     string // shown in messages as {sender_name}
	By       string // the user acting, for the audit fields
	Platform bool   // a platform admin may set class freely
}

// ValidationError is a problem with what the sender wrote (a 400 for the caller).
type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Invalid wraps err as a ValidationError.
func Invalid(err error) error {
	if err == nil {
		return nil
	}
	return &ValidationError{Err: err}
}

// Errors.
var (
	ErrNotFound      = errors.New("broadcast not found")
	ErrNotEditable   = errors.New("only drafts and broadcasts awaiting approval can be edited")
	ErrBadTransition = errors.New("this action is not allowed in the broadcast's current state")
)

// Service manages broadcasts.
type Service struct {
	client *ent.Client
	log    *zap.Logger
}

// NewService builds the service.
func NewService(client *ent.Client, log *zap.Logger) *Service {
	return &Service{client: client, log: log.Named("broadcasts")}
}

// classFor is the consent class of a kind. Only the platform may mark a greeting or offer as
// transactional; a service notice is transactional by nature.
func classFor(kind, requested string, platform bool) string {
	if platform && (requested == "marketing" || requested == "transactional") {
		return requested
	}
	if kind == "service_notice" {
		return "transactional"
	}
	return "marketing"
}

// Validate checks an input for one sender.
func (s *Service) Validate(in Input, sender Sender) error {
	if strings.TrimSpace(in.Title) == "" {
		return errors.New("title is required")
	}
	if len(in.Channels) == 0 {
		return errors.New("choose at least one channel")
	}
	texts := map[string]string{}
	for _, c := range in.Channels {
		if !knownChannels[c] {
			return fmt.Errorf("unknown channel %q", c)
		}
		switch c {
		case "email":
			if in.Content.Email == nil || strings.TrimSpace(in.Content.Email.Subject) == "" || strings.TrimSpace(in.Content.Email.Body) == "" {
				return errors.New("email needs a subject and a message")
			}
			texts["email.subject"], texts["email.body"] = in.Content.Email.Subject, in.Content.Email.Body
		case "sms":
			if in.Content.SMS == nil || strings.TrimSpace(in.Content.SMS.Body) == "" {
				return errors.New("SMS needs a message")
			}
			texts["sms.body"] = in.Content.SMS.Body
		case "whatsapp":
			w := in.Content.WhatsApp
			if w == nil || strings.TrimSpace(w.Template) == "" {
				return errors.New("WhatsApp needs an approved template")
			}
			for _, p := range w.Params {
				if !whatsappParamTokens[p] {
					return fmt.Errorf("WhatsApp template value %q is not one of first_name, name, business_name, sender_name, year, occasion, message", p)
				}
				if p == "message" && strings.TrimSpace(w.Message) == "" {
					return errors.New("the WhatsApp template uses the message text, which is empty")
				}
			}
			texts["whatsapp.message"] = w.Message
			if strings.Contains(w.Message, "://") {
				return errors.New("WhatsApp messages cannot contain links; links go on template buttons")
			}
		case "push":
			if in.Content.Push == nil || strings.TrimSpace(in.Content.Push.Body) == "" {
				return errors.New("push needs a message")
			}
			texts["push.title"], texts["push.body"] = in.Content.Push.Title, in.Content.Push.Body
		case "in_app":
			if in.Content.InApp == nil || strings.TrimSpace(in.Content.InApp.Title) == "" || strings.TrimSpace(in.Content.InApp.Summary) == "" {
				return errors.New("the banner needs a title and a summary")
			}
		}
	}
	if err := ValidateText(texts); err != nil {
		return err
	}
	aType, _ := in.Audience["type"].(string)
	switch aType {
	case AudiencePlatformTenants:
		if !sender.Platform {
			return errors.New("only the platform can message all tenants")
		}
	case AudienceTenantCustomers, AudienceTenantUsers:
		if sender.Platform && sender.TenantID == nil {
			return errors.New("the platform messages tenants; choose the tenants audience")
		}
	case "":
		if !(len(in.Channels) == 1 && in.Channels[0] == "in_app") {
			return errors.New("choose who receives it")
		}
	default:
		return fmt.Errorf("unknown audience %q", aType)
	}
	if in.Timezone != "" {
		if _, err := time.LoadLocation(in.Timezone); err != nil {
			return fmt.Errorf("unknown timezone %q", in.Timezone)
		}
	}
	return nil
}

// Create saves a new draft.
func (s *Service) Create(ctx context.Context, in Input, sender Sender) (*ent.Broadcast, error) {
	if err := s.Validate(in, sender); err != nil {
		return nil, Invalid(err)
	}
	kind := in.Kind
	if kind == "" {
		kind = "announcement"
	}
	scope := entbroadcast.ScopeTenant
	if sender.TenantID == nil {
		scope = entbroadcast.ScopePlatform
	}
	c := s.client.Broadcast.Create().
		SetScope(scope).
		SetKind(entbroadcast.Kind(kind)).
		SetClass(entbroadcast.Class(classFor(kind, in.Class, sender.Platform))).
		SetTitle(strings.TrimSpace(in.Title)).
		SetChannels(in.Channels).
		SetContent(contentMap(in.Content)).
		SetAudience(in.Audience).
		SetNillableSendAt(in.SendAt).
		SetRequestedBy(sender.By).
		SetMetadata(senderMeta(nil, in, sender))
	if sender.TenantID != nil {
		c.SetTenantID(*sender.TenantID)
	}
	return c.Save(ctx)
}

// Update edits a draft or a broadcast awaiting approval (which then needs approval again).
func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input, sender Sender) (*ent.Broadcast, error) {
	b, err := s.Get(ctx, id, sender.TenantID)
	if err != nil {
		return nil, err
	}
	if b.Status != entbroadcast.StatusDraft && b.Status != entbroadcast.StatusPendingApproval {
		return nil, ErrNotEditable
	}
	if err := s.Validate(in, sender); err != nil {
		return nil, Invalid(err)
	}
	kind := in.Kind
	if kind == "" {
		kind = string(b.Kind)
	}
	up := b.Update().
		SetKind(entbroadcast.Kind(kind)).
		SetClass(entbroadcast.Class(classFor(kind, in.Class, sender.Platform))).
		SetTitle(strings.TrimSpace(in.Title)).
		SetChannels(in.Channels).
		SetContent(contentMap(in.Content)).
		SetAudience(in.Audience).
		SetMetadata(senderMeta(b.Metadata, in, sender))
	if in.SendAt != nil {
		up.SetSendAt(*in.SendAt)
	} else {
		up.ClearSendAt()
	}
	return up.Save(ctx)
}

// Get loads a broadcast the sender owns (platform: platform rows only; tenant: its own).
func (s *Service) Get(ctx context.Context, id uuid.UUID, tenantID *uuid.UUID) (*ent.Broadcast, error) {
	q := s.client.Broadcast.Query().Where(entbroadcast.ID(id))
	if tenantID == nil {
		q = q.Where(entbroadcast.TenantIDIsNil())
	} else {
		q = q.Where(entbroadcast.TenantID(*tenantID))
	}
	b, err := q.Only(ctx)
	if ent.IsNotFound(err) {
		return nil, ErrNotFound
	}
	return b, err
}

// ListFilter narrows a list.
type ListFilter struct {
	Status   []string
	Offset   int
	Limit    int
	Occasion bool // only occasion broadcasts
}

// List pages the sender's broadcasts, newest first.
func (s *Service) List(ctx context.Context, tenantID *uuid.UUID, f ListFilter) ([]*ent.Broadcast, int, error) {
	q := s.client.Broadcast.Query()
	if tenantID == nil {
		q = q.Where(entbroadcast.TenantIDIsNil())
	} else {
		q = q.Where(entbroadcast.TenantID(*tenantID))
	}
	if len(f.Status) > 0 {
		st := make([]entbroadcast.Status, 0, len(f.Status))
		for _, v := range f.Status {
			st = append(st, entbroadcast.Status(v))
		}
		q = q.Where(entbroadcast.StatusIn(st...))
	}
	if f.Occasion {
		q = q.Where(entbroadcast.OccasionIDNotNil())
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	rows, err := q.Order(ent.Desc(entbroadcast.FieldCreatedAt)).Offset(f.Offset).Limit(f.Limit).All(ctx)
	return rows, total, err
}

// Delete removes a draft (anything else is cancelled instead, to keep its history).
func (s *Service) Delete(ctx context.Context, id uuid.UUID, tenantID *uuid.UUID) error {
	b, err := s.Get(ctx, id, tenantID)
	if err != nil {
		return err
	}
	if b.Status != entbroadcast.StatusDraft && b.Status != entbroadcast.StatusRejected {
		return ErrNotEditable
	}
	return s.client.Broadcast.DeleteOne(b).Exec(ctx)
}

// Transition actions.
const (
	ActionSubmit  = "submit"
	ActionApprove = "approve"
	ActionReject  = "reject"
	ActionPause   = "pause"
	ActionResume  = "resume"
	ActionCancel  = "cancel"
)

// Act applies an approval or delivery action. Approve freezes the content and schedules it
// (send_at, or now). Only rows still in the expected state change, so two admins clicking at
// once cannot double-approve.
func (s *Service) Act(ctx context.Context, id uuid.UUID, action string, sender Sender, note string) (*ent.Broadcast, error) {
	b, err := s.Get(ctx, id, sender.TenantID)
	if err != nil {
		return nil, err
	}
	from := map[string][]entbroadcast.Status{
		ActionSubmit:  {entbroadcast.StatusDraft, entbroadcast.StatusRejected},
		ActionApprove: {entbroadcast.StatusDraft, entbroadcast.StatusPendingApproval},
		ActionReject:  {entbroadcast.StatusPendingApproval},
		ActionPause:   {entbroadcast.StatusScheduled, entbroadcast.StatusSending},
		ActionResume:  {entbroadcast.StatusPaused},
		ActionCancel:  {entbroadcast.StatusDraft, entbroadcast.StatusPendingApproval, entbroadcast.StatusScheduled, entbroadcast.StatusSending, entbroadcast.StatusPaused},
	}[action]
	if from == nil {
		return nil, Invalid(fmt.Errorf("unknown action %q", action))
	}
	ok := false
	for _, st := range from {
		if b.Status == st {
			ok = true
		}
	}
	if !ok {
		return nil, ErrBadTransition
	}
	if action == ActionApprove {
		if err := s.Validate(inputFrom(b), sender); err != nil {
			return nil, Invalid(err)
		}
	}

	meta := cloneMap(b.Metadata)
	up := s.client.Broadcast.Update().Where(entbroadcast.ID(b.ID), entbroadcast.StatusEQ(b.Status))
	now := time.Now()
	switch action {
	case ActionSubmit:
		up.SetStatus(entbroadcast.StatusPendingApproval)
	case ActionApprove:
		up.SetStatus(entbroadcast.StatusScheduled).SetApprovedBy(sender.By).SetApprovedAt(now)
		if b.SendAt == nil || b.SendAt.Before(now) {
			up.SetSendAt(now)
		}
	case ActionReject:
		up.SetStatus(entbroadcast.StatusRejected)
		meta["rejection_note"] = note
	case ActionPause:
		up.SetStatus(entbroadcast.StatusPaused)
		meta["paused_from"] = string(b.Status)
	case ActionResume:
		resumeTo := entbroadcast.StatusScheduled
		if v, _ := meta["paused_from"].(string); v == string(entbroadcast.StatusSending) {
			resumeTo = entbroadcast.StatusSending
		}
		up.SetStatus(resumeTo)
		delete(meta, "paused_from")
	case ActionCancel:
		up.SetStatus(entbroadcast.StatusCancelled).SetCompletedAt(now)
	}
	if note != "" || action == ActionReject || action == ActionPause || action == ActionResume {
		up.SetMetadata(meta)
	}
	n, err := up.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, ErrBadTransition
	}
	if action == ActionCancel {
		// Anything not yet handed to a provider stays unsent.
		_, _ = s.client.BroadcastRecipient.Update().
			Where(entrecipient.BroadcastID(b.ID), entrecipient.StatusEQ(entrecipient.StatusPending)).
			SetStatus(entrecipient.StatusSkipped).SetError("broadcast cancelled").Save(ctx)
	}
	return s.client.Broadcast.Get(ctx, b.ID)
}

// RecipientPage lists a broadcast's recipients (addresses masked by the caller).
func (s *Service) RecipientPage(ctx context.Context, broadcastID uuid.UUID, status, channel string, offset, limit int) ([]*ent.BroadcastRecipient, int, error) {
	q := s.client.BroadcastRecipient.Query().Where(entrecipient.BroadcastID(broadcastID))
	if status != "" {
		q = q.Where(entrecipient.StatusEQ(entrecipient.Status(status)))
	}
	if channel != "" {
		q = q.Where(entrecipient.Channel(channel))
	}
	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := q.Order(ent.Asc(entrecipient.FieldID)).Offset(offset).Limit(limit).All(ctx)
	return rows, total, err
}

// ChannelCounts is the per-channel, per-status breakdown for the detail view (one GROUP BY).
func (s *Service) ChannelCounts(ctx context.Context, broadcastID uuid.UUID) (map[string]map[string]int, error) {
	var rows []struct {
		Channel string `json:"channel"`
		Status  string `json:"status"`
		Count   int    `json:"count"`
	}
	err := s.client.BroadcastRecipient.Query().Where(entrecipient.BroadcastID(broadcastID)).
		GroupBy(entrecipient.FieldChannel, entrecipient.FieldStatus).Aggregate(ent.Count()).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]int{}
	for _, r := range rows {
		if out[r.Channel] == nil {
			out[r.Channel] = map[string]int{}
		}
		out[r.Channel][r.Status] = r.Count
	}
	return out, nil
}

// RefreshCounts recomputes a broadcast's counters from its recipients in one aggregate query.
func RefreshCounts(ctx context.Context, client *ent.Client, broadcastID uuid.UUID) error {
	var rows []struct {
		Status string `json:"status"`
		Count  int    `json:"count"`
	}
	if err := client.BroadcastRecipient.Query().Where(entrecipient.BroadcastID(broadcastID)).
		GroupBy(entrecipient.FieldStatus).Aggregate(ent.Count()).Scan(ctx, &rows); err != nil {
		return err
	}
	var total, sent, failed, skipped, suppressed int
	for _, r := range rows {
		total += r.Count
		switch r.Status {
		case "sent", "delivered":
			sent += r.Count
		case "failed":
			failed += r.Count
		case "skipped":
			skipped += r.Count
		case "suppressed":
			suppressed += r.Count
		}
	}
	return client.Broadcast.UpdateOneID(broadcastID).
		SetTargetCount(total).SetSentCount(sent).SetFailedCount(failed).
		SetSkippedCount(skipped).SetSuppressedCount(suppressed).Exec(ctx)
}

// LastSender finds who most recently broadcast to an address on a channel, so a STOP reply
// opts out of that sender (a tenant may send through the platform's shared number). It also
// returns the stored address when it is still kept. found is false when no broadcast reached it.
func LastSender(ctx context.Context, client *ent.Client, channel, addressHash string) (tenantID *uuid.UUID, address string, found bool) {
	r, err := client.BroadcastRecipient.Query().
		Where(entrecipient.Channel(channel), entrecipient.AddressHash(addressHash)).
		Order(ent.Desc(entrecipient.FieldCreatedAt)).First(ctx)
	if err != nil {
		return nil, "", false
	}
	if r.Address != nil {
		address = *r.Address
	}
	return r.TenantID, address, true
}

// PendingApprovalCount is the badge on the Broadcasts menu.
func (s *Service) PendingApprovalCount(ctx context.Context, tenantID *uuid.UUID) (int, error) {
	q := s.client.Broadcast.Query().Where(entbroadcast.StatusEQ(entbroadcast.StatusPendingApproval))
	if tenantID == nil {
		q = q.Where(entbroadcast.TenantIDIsNil())
	} else {
		q = q.Where(entbroadcast.TenantID(*tenantID))
	}
	return q.Count(ctx)
}

// ContentOf decodes a broadcast's content column.
func ContentOf(b *ent.Broadcast) Content {
	var c Content
	raw, _ := json.Marshal(b.Content)
	_ = json.Unmarshal(raw, &c)
	return c
}

func inputFrom(b *ent.Broadcast) Input {
	in := Input{Title: b.Title, Kind: string(b.Kind), Class: string(b.Class), Channels: b.Channels, Content: ContentOf(b), Audience: b.Audience, SendAt: b.SendAt}
	in.SenderName, _ = b.Metadata["sender_name"].(string)
	in.Timezone, _ = b.Metadata["timezone"].(string)
	in.ConsentAttested, _ = b.Metadata["consent_attested"].(bool)
	return in
}

func contentMap(c Content) map[string]any {
	raw, _ := json.Marshal(c)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func senderMeta(current map[string]any, in Input, sender Sender) map[string]any {
	m := cloneMap(current)
	name := strings.TrimSpace(in.SenderName)
	if name == "" {
		name = sender.Name
	}
	m["sender_name"] = name
	tz := in.Timezone
	if tz == "" {
		if v, _ := m["timezone"].(string); v != "" {
			tz = v
		} else {
			tz = "Africa/Nairobi"
		}
	}
	m["timezone"] = tz
	m["consent_attested"] = in.ConsentAttested
	return m
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}
