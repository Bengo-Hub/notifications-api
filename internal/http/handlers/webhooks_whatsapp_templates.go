package handlers

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware/pii"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent/deliverylog"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatecat"
)

// waTemplateEvent is the value of Meta's template webhooks:
//   - message_template_status_update: event (APPROVED, REJECTED, PAUSED, DISABLED, FLAGGED, ...)
//     and reason, for message_template_name;
//   - template_category_update: previous_category and new_category (or correct_category, on the
//     notice Meta sends before a move takes effect).
type waTemplateEvent struct {
	Event                   string `json:"event"`
	MessageTemplateName     string `json:"message_template_name"`
	MessageTemplateLanguage string `json:"message_template_language"`
	Reason                  string `json:"reason"`
	PreviousCategory        string `json:"previous_category"`
	NewCategory             string `json:"new_category"`
	CorrectCategory         string `json:"correct_category"`
}

// unreachableFor is how long a number Meta could not deliver to (131026) is skipped on WhatsApp.
// The person can fix the cause (install or update WhatsApp, accept Meta's terms), so it lapses.
const unreachableFor = 7 * 24 * time.Hour

// metaErrUnreachable is Meta's "message undeliverable" (131026).
const metaErrUnreachable = 131026

// WithTemplateCategories keeps Meta's template categories and send outcomes in store, and tells
// ops about moves and review outcomes through alert. Either may be nil.
func (h *WebhookHandler) WithTemplateCategories(store *templatecat.Store, alert templatecat.Alerter) *WebhookHandler {
	h.templateCats = store
	h.templateAlert = alert
	return h
}

// onTemplateEvent records a template webhook and alerts ops once, even when Meta sends it twice
// or two replicas receive it.
func (h *WebhookHandler) onTemplateEvent(ctx context.Context, field, wabaID string, v waTemplateEvent) {
	name := v.MessageTemplateName
	if name == "" {
		return
	}
	c := templatecat.Change{WABAID: wabaID, Name: name, Language: v.MessageTemplateLanguage, Source: "webhook"}
	switch field {
	case "template_category_update":
		newCat := strings.ToUpper(strings.TrimSpace(firstNonBlank(v.NewCategory, v.CorrectCategory)))
		if newCat == "" {
			return
		}
		prev := strings.ToUpper(v.PreviousCategory)
		if stored, _, err := h.templateCats.Set(ctx, wabaID, name, newCat); err != nil {
			h.log.Warn("whatsapp template category: could not record it", zap.String("template", name), zap.Error(err))
		} else if prev == "" {
			prev = stored
		}
		c.Kind, c.PreviousCategory, c.NewCategory = "category", prev, newCat
		h.log.Warn("whatsapp template category changed by Meta", zap.String("waba_id", wabaID), zap.String("template", name),
			zap.String("previous_category", prev), zap.String("new_category", newCat))
	default:
		status := strings.ToUpper(v.Event)
		if status == "" {
			return
		}
		c.Kind, c.Status, c.Reason = "status", status, v.Reason
		h.log.Info("whatsapp template review status", zap.String("waba_id", wabaID), zap.String("template", name),
			zap.String("status", status), zap.String("reason", v.Reason))
	}
	if h.templateAlert == nil {
		return
	}
	key := strings.Join([]string{c.Kind, wabaID, name, c.NewCategory, c.Status}, "|")
	if !h.templateCats.Claim(ctx, key, 0) {
		return
	}
	h.templateAlert.AlertWhatsAppTemplateChange(context.WithoutCancel(ctx), c)
}

// onSendOutcome records Meta's later verdict on a send that its API had accepted: for the Test
// button (Redis, an hour), on the delivery log row (Monitoring shows failed with Meta's reason
// instead of "sent"), and for 131026 a week's WhatsApp suppression of that number for the sender.
func (h *WebhookHandler) onSendOutcome(ctx context.Context, tenantID, recipient string, st templatecat.SendStatus) {
	if st.MessageID == "" {
		return
	}
	h.templateCats.RecordSendStatus(ctx, st)
	h.recordDeliveryOutcome(ctx, st)
	if st.Status == "failed" && st.Code == metaErrUnreachable && recipient != "" && h.optOut != nil {
		var tid *uuid.UUID
		if u, err := uuid.Parse(tenantID); err == nil {
			tid = &u
		}
		until := time.Now().Add(unreachableFor)
		if err := h.optOut.AddUntil(ctx, tid, "whatsapp", pii.HashAddress(recipient), suppression.ReasonUnreachable, "whatsapp_status", until); err != nil {
			h.log.Warn("whatsapp: could not pause an unreachable number", zap.Error(err))
		} else {
			h.log.Info("whatsapp: number Meta could not reach is skipped for a week", pii.Phone("recipient", recipient),
				zap.String("tenant_id", tenantID))
		}
	}
}

// recordDeliveryOutcome moves the delivery log row of a send to Meta's final status. A failure is
// final; otherwise the status only moves forward (Meta can report "sent" after "delivered").
func (h *WebhookHandler) recordDeliveryOutcome(ctx context.Context, st templatecat.SendStatus) {
	if h.client == nil {
		return
	}
	q := h.client.DeliveryLog.Update().Where(deliverylog.ProviderMessageID(st.MessageID))
	switch st.Status {
	case "failed":
		q = q.SetStatus("failed")
		if st.Code != 0 {
			q = q.SetErrorCode(strconv.Itoa(st.Code))
		}
		if d := firstNonBlank(st.Detail, st.Title); d != "" {
			q = q.SetErrorDetail(d)
		}
	case "delivered":
		q = q.Where(deliverylog.StatusIn("sent")).SetStatus("delivered")
	case "read":
		q = q.Where(deliverylog.StatusIn("sent", "delivered")).SetStatus("read")
	default:
		return
	}
	if _, err := q.Save(ctx); err != nil {
		h.log.Warn("whatsapp: could not record the delivery outcome", zap.String("message_id", st.MessageID), zap.Error(err))
	}
}
