package handlers

import (
	"context"
	"time"

	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/modules/broadcasts"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// SubjectWhatsAppInbound carries messages sent to the platform WhatsApp number to MarketFlow's
// WhatsApp assistant (Sprint 17). MarketFlow owns who the sender is and whether the text is for
// the assistant; this service only hands the message over, with no tenant in it, because a
// platform-number message belongs to no tenant until MarketFlow matches the sender.
const SubjectWhatsAppInbound = "notifications.whatsapp.inbound"

// WithAssistantInbound publishes platform-number messages on SubjectWhatsAppInbound.
func (h *WebhookHandler) WithAssistantInbound(nc *nats.Conn) *WebhookHandler {
	h.assistantNC = nc
	return h
}

// isPlatformNumber reports whether phoneNumberID is the platform's own WhatsApp number (the
// "platform" ProviderSetting row), as opposed to a tenant's number.
func (h *WebhookHandler) isPlatformNumber(ctx context.Context, phoneNumberID string) bool {
	if phoneNumberID == "" || h.client == nil {
		return false
	}
	ok, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID("platform"),
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.Key("phone_number_id"),
			providersetting.Value(phoneNumberID),
		).
		Exist(ctx)
	return err == nil && ok
}

// assistantInbound is the event body; the wa_message_id is the consumer's dedupe key.
func assistantInbound(phoneNumberID string, m waInboundMessage, text, contactName string) map[string]any {
	return map[string]any{
		"phone_number_id": phoneNumberID,
		"wa_message_id":   m.ID,
		"from":            m.From,
		"type":            m.Type,
		"text":            text,
		"contact_name":    contactName,
		"received_at":     time.Now().UTC().Format(time.RFC3339),
	}
}

// publishAssistantInbound hands one platform-number message to MarketFlow. A STOP reply and a
// message with no text (media, location) are not for the assistant. Best effort: the message is
// still acked to Meta, and a lost event costs one unanswered message, never a wrong action.
func (h *WebhookHandler) publishAssistantInbound(ctx context.Context, tenantID, phoneNumberID string, m waInboundMessage, text, contactName string) {
	if h.assistantNC == nil || tenantID != "" || text == "" || suppression.IsStopReply(text) {
		return
	}
	if !h.isPlatformNumber(ctx, phoneNumberID) {
		return
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return
	}
	broadcasts.PublishEvent(h.assistantNC, h.log, SubjectWhatsAppInbound, "whatsapp.inbound", nil, id,
		assistantInbound(phoneNumberID, m, text, contactName))
}
