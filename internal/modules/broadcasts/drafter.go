package broadcasts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	entbroadcast "github.com/bengobox/notifications-api/internal/ent/broadcast"
	"github.com/bengobox/notifications-api/internal/modules/occasions"
)

// Drafter turns occasions into broadcast drafts. The worker's planner runs it daily; the API runs
// it when a sender asks for this year's draft now.
type Drafter struct {
	Client     *ent.Client
	Occasions  *occasions.Service
	PlatformID string
	Log        *zap.Logger
}

// ErrNoOccurrence means the occasion has no date this year that is still ahead or under way.
var ErrNoOccurrence = errors.New("this occasion has no upcoming date this year")

// Plan creates this year's broadcast for every enabled occasion whose send date is within its
// lead time. The unique (sender, occasion, year) index makes it safe to run on every pod.
func (dr *Drafter) Plan(ctx context.Context) error {
	loc := mustLoc("Africa/Nairobi")
	due, err := dr.Occasions.DueSoon(ctx, time.Now(), loc)
	if err != nil {
		return err
	}
	created := 0
	for _, d := range due {
		_, ok, err := dr.Draft(ctx, d, loc)
		if err != nil {
			dr.Log.Warn("occasion draft failed", zap.String("occasion", d.View.Key), zap.Error(err))
			continue
		}
		if ok {
			created++
		}
	}
	if created > 0 {
		dr.Log.Info("occasion drafts created", zap.Int("count", created))
	}
	return nil
}

// DraftNow creates (or returns) this year's broadcast for one of the sender's occasions, even
// before its lead time, and also while the occasion is under way (Customer Service Week runs five
// days, so a greeting on day three is still on time).
func (dr *Drafter) DraftNow(ctx context.Context, tenantID *uuid.UUID, key string) (*ent.Broadcast, error) {
	loc := mustLoc("Africa/Nairobi")
	now := time.Now().In(loc)
	v, err := dr.Occasions.Get(ctx, tenantID, key, now, loc)
	if err != nil {
		return nil, err
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start, err := v.Rule.DateIn(now.Year(), loc)
	if err != nil || !today.Before(start.AddDate(0, 0, max(v.DurationDays, 1))) {
		return nil, ErrNoOccurrence
	}
	sendOn := v.SendDate(start)
	if sendOn.Before(today) {
		sendOn = today
	}
	b, _, err := dr.Draft(ctx, occasions.Due{View: v, TenantID: tenantID, Start: start, SendOn: sendOn, Year: start.Year()}, loc)
	return b, err
}

// Draft creates the broadcast for one occasion occurrence, or returns the one already made.
// created is false when it already existed.
func (dr *Drafter) Draft(ctx context.Context, d occasions.Due, loc *time.Location) (*ent.Broadcast, bool, error) {
	q := dr.Client.Broadcast.Query().Where(entbroadcast.OccasionID(d.View.ID), entbroadcast.OccasionYear(d.Year))
	if d.TenantID == nil {
		q = q.Where(entbroadcast.TenantIDIsNil())
	} else {
		q = q.Where(entbroadcast.TenantID(*d.TenantID))
	}
	if existing, err := q.First(ctx); err == nil {
		return existing, false, nil
	} else if !ent.IsNotFound(err) {
		return nil, false, err
	}
	in, err := dr.occasionInput(ctx, d, loc)
	if err != nil {
		return nil, false, err
	}
	status := entbroadcast.StatusPendingApproval
	if d.View.Settings.AutoSend {
		status = entbroadcast.StatusScheduled
	}
	scope := entbroadcast.ScopePlatform
	if d.TenantID != nil {
		scope = entbroadcast.ScopeTenant
	}
	meta := senderMeta(nil, in, Sender{Name: in.SenderName})
	_, variantIdx := occasions.PickVariant(d.View.Settings.Variants, d.Year)
	meta["variant_index"] = variantIdx
	meta["occasion_key"] = d.View.Key
	meta["occasion_name"] = d.View.Name
	meta["generated"] = true
	c := dr.Client.Broadcast.Create().
		SetScope(scope).SetKind(entbroadcast.KindGreeting).SetClass(entbroadcast.ClassMarketing).
		SetStatus(status).SetTitle(in.Title).SetChannels(in.Channels).SetContent(contentMap(in.Content)).
		SetAudience(in.Audience).SetSendAt(*in.SendAt).SetOccasionID(d.View.ID).SetOccasionYear(d.Year).
		SetRequestedBy("occasion planner").SetMetadata(meta)
	if d.TenantID != nil {
		c.SetTenantID(*d.TenantID)
	}
	if status == entbroadcast.StatusScheduled {
		c.SetApprovedBy("auto-send").SetApprovedAt(time.Now())
	}
	b, err := c.Save(ctx)
	if err != nil {
		if ent.IsConstraintError(err) { // another pod made it first
			existing, qerr := q.First(ctx)
			return existing, false, qerr
		}
		return nil, false, err
	}
	return b, true, nil
}

// occasionInput builds the draft for one occasion and year: the year's wording, the sender's
// channels and audience, and the send time on the send day.
func (dr *Drafter) occasionInput(ctx context.Context, d occasions.Due, loc *time.Location) (Input, error) {
	v, _ := occasions.PickVariant(d.View.Settings.Variants, d.Year)
	if strings.TrimSpace(v.Body) == "" {
		return Input{}, errors.New("occasion has no wording")
	}
	senderName := dr.SenderName(ctx, d.TenantID)
	channels := d.View.Settings.Channels
	if len(channels) == 0 {
		channels = []string{"email"}
	}
	content := Content{}
	var kept []string
	for _, ch := range channels {
		switch ch {
		case "email":
			subject := v.Subject
			if subject == "" {
				subject = d.View.Name + " greetings from {sender_name}"
			}
			content.Email = &EmailContent{Subject: subject, Body: v.Body}
		case "sms":
			body := v.SMS
			if body == "" {
				body = v.Body
			}
			content.SMS = &SMSContent{Body: body}
		case "whatsapp":
			tpl := d.View.Settings.WhatsAppTemplate
			params, ok := OccasionTemplateParams[tpl]
			if tpl == "" || !ok {
				continue // no approved template for this occasion: email and SMS still go
			}
			content.WhatsApp = &WhatsAppContent{Template: tpl, Params: params}
		default:
			continue
		}
		kept = append(kept, ch)
	}
	if len(kept) == 0 {
		return Input{}, errors.New("occasion has no usable channel")
	}
	audience := map[string]any{"type": AudiencePlatformTenants}
	if d.TenantID != nil {
		a := d.View.Settings.Audience
		if a == "" {
			a = AudienceTenantCustomers
		}
		audience = map[string]any{"type": a}
		if d.View.Settings.SegmentID != "" {
			audience["segment_id"] = d.View.Settings.SegmentID
		}
	}
	sendTime := d.View.Settings.SendTime
	if sendTime == "" {
		sendTime = "09:00"
	}
	hm, _ := time.Parse("15:04", sendTime)
	sendAt := time.Date(d.SendOn.Year(), d.SendOn.Month(), d.SendOn.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if sendAt.Before(time.Now()) {
		sendAt = time.Now()
	}
	return Input{
		Title:      fmt.Sprintf("%s %d", d.View.Name, d.Year),
		Kind:       "greeting",
		Channels:   kept,
		Content:    content,
		Audience:   audience,
		SendAt:     &sendAt,
		SenderName: senderName,
		Timezone:   loc.String(),
	}, nil
}

// OccasionTemplateParams maps each approved WhatsApp template to the values filling {{1}}, {{2}}...
var OccasionTemplateParams = map[string][]string{
	"occasion_customer_service_week_v1": {"first_name", "sender_name"},
	"occasion_new_year_v1":              {"first_name", "year", "sender_name"},
	"occasion_idd_v1":                   {"first_name", "occasion", "sender_name"},
	"occasion_easter_v1":                {"first_name", "sender_name"},
	"occasion_labour_day_v1":            {"first_name", "sender_name"},
	"occasion_national_day_v1":          {"first_name", "occasion", "sender_name"},
	"occasion_christmas_v1":             {"first_name", "sender_name"},
	"broadcast_update_v1":               {"first_name", "sender_name", "message"},
	"broadcast_service_notice_v1":       {"first_name", "sender_name", "message"},
}

// SenderName is how messages sign off: the sending tenant's name, or the platform's.
func (dr *Drafter) SenderName(ctx context.Context, tenantID *uuid.UUID) string {
	id := dr.PlatformID
	if tenantID != nil {
		id = tenantID.String()
	}
	if tid, err := uuid.Parse(id); err == nil {
		if t, err := dr.Client.Tenant.Get(ctx, tid); err == nil && t.Name != "" {
			return t.Name
		}
	}
	return "Codevertex Africa"
}
