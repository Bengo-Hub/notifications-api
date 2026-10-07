package handlers

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware/contact"
	"github.com/Bengo-Hub/httpware/pii"

	"github.com/bengobox/notifications-api/internal/modules/broadcasts"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
)

// WithOptOut lets inbound messages (a STOP text, the "Stop promotions" quick reply) opt the
// sender's number out of marketing. Without it, inbound messages are only recorded.
func (h *WebhookHandler) WithOptOut(s *suppression.Service) *WebhookHandler {
	h.optOut = s
	return h
}

// handleStopReply opts a phone out of a sender's marketing when the inbound text or button asks
// to stop. The sender is whoever last broadcast to that number on the channel; with no such
// broadcast it is the owner of the receiving number (ownerTenant, "" for the platform's).
func (h *WebhookHandler) handleStopReply(ctx context.Context, channel, from, text, ownerTenant, source string) bool {
	if h.optOut == nil || !suppression.IsStopReply(text) {
		return false
	}
	phone, err := contact.NormalizePhone(from, "")
	if err != nil {
		if from == "" {
			return false
		}
		phone = "+" + from // WhatsApp sends international digits without the plus
	}
	hash := pii.HashAddress(phone)
	sender, _, found := broadcasts.LastSender(ctx, h.client, channel, hash)
	if !found && ownerTenant != "" {
		if id, err := uuid.Parse(ownerTenant); err == nil {
			sender = &id
		}
	}
	if err := h.optOut.OptOut(ctx, sender, channel, phone, hash, "stop_reply", source); err != nil {
		h.log.Warn("opt-out from inbound reply failed", zap.String("channel", channel), zap.Error(err))
		return false
	}
	h.log.Info("recipient opted out by reply", zap.String("channel", channel), pii.Phone("from", phone))
	return true
}

// AfricasTalkingInbound receives incoming SMS (form-encoded: from, to, text, date, id, linkId).
// A STOP/ACHA/UNSUBSCRIBE reply opts the number out of that sender's marketing SMS. Africa's
// Talking needs a 200 or it retries.
func (h *WebhookHandler) AfricasTalkingInbound(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	h.handleStopReply(r.Context(), "sms", r.FormValue("from"), r.FormValue("text"), "", "sms_inbound")
	w.WriteHeader(http.StatusOK)
}
