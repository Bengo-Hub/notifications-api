package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
)

// WebhookHandler receives provider-initiated callbacks (SMS delivery reports, WhatsApp message/
// status webhooks) — all public, unauthenticated routes, matching treasury-api's
// /webhooks/{provider}/... convention for the same reason: the provider calls these directly with
// no tenant JWT to attach.
type WebhookHandler struct {
	client *ent.Client
	log    *zap.Logger
}

// NewWebhookHandler creates the webhook handler.
func NewWebhookHandler(client *ent.Client, log *zap.Logger) *WebhookHandler {
	return &WebhookHandler{client: client, log: log.Named("webhooks")}
}

// AfricasTalkingDLR receives Africa's Talking's SMS delivery report callback (form-encoded: id,
// status, phoneNumber, networkCode, failureReason, retryCount) once a message's final carrier
// status is known. Logged for visibility only for now — not yet correlated back to a delivery_logs
// row, since that table doesn't store AT's message id today. AT requires a 200 or it retries.
func (h *WebhookHandler) AfricasTalkingDLR(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	h.log.Info("africastalking delivery report",
		zap.String("message_id", r.FormValue("id")),
		zap.String("status", r.FormValue("status")),
		zap.String("phone_number", r.FormValue("phoneNumber")),
		zap.String("network_code", r.FormValue("networkCode")),
		zap.String("failure_reason", r.FormValue("failureReason")),
		zap.String("retry_count", r.FormValue("retryCount")),
	)
	w.WriteHeader(http.StatusOK)
}

// metaWebhookVerifyToken is the shared secret Meta echoes back during the webhook subscription
// handshake (WhatsApp Manager → Configuration → Webhooks → Verify Token must match this exactly).
// One value for the whole app/WABA — not per-tenant, since Meta's verification handshake happens
// once per App, before any tenant-specific routing is even relevant.
func metaWebhookVerifyToken() string {
	if v := os.Getenv("WHATSAPP_WEBHOOK_VERIFY_TOKEN"); v != "" {
		return v
	}
	return "codevertex-whatsapp-webhook"
}

// WhatsAppVerify handles Meta's webhook subscription handshake (GET with hub.mode/hub.verify_token/
// hub.challenge query params) — required once, when registering the Callback URL in WhatsApp
// Manager → Configuration → Webhooks. Meta requires an EXACT echo of hub.challenge as the raw
// response body (no JSON wrapping) when hub.mode=="subscribe" and the verify token matches.
func (h *WebhookHandler) WhatsAppVerify(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("hub.mode")
	token := r.URL.Query().Get("hub.verify_token")
	challenge := r.URL.Query().Get("hub.challenge")

	if mode == "subscribe" && token == metaWebhookVerifyToken() && challenge != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(challenge))
		h.log.Info("whatsapp webhook verified")
		return
	}
	h.log.Warn("whatsapp webhook verification failed", zap.String("mode", mode))
	w.WriteHeader(http.StatusForbidden)
}

// waWebhookPayload is the subset of Meta's WhatsApp webhook notification shape this handler reads.
// Full reference: https://developers.facebook.com/docs/whatsapp/cloud-api/webhooks/components
type waWebhookPayload struct {
	Entry []struct {
		ID      string `json:"id"` // WABA ID
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
				} `json:"messages"`
				Statuses []struct {
					ID          string `json:"id"`
					Status      string `json:"status"` // sent | delivered | read | failed
					RecipientID string `json:"recipient_id"`
				} `json:"statuses"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// WhatsAppIncoming receives Meta's WhatsApp webhook notifications (incoming customer messages and
// outbound message status updates: sent/delivered/read/failed). Routes each notification to the
// tenant that owns the receiving phone_number_id — every tenant can register their own WhatsApp
// number (settings → providers → WhatsApp), added to the same Meta app/WABA the platform number
// lives in, so a single webhook subscription covers every tenant's number; this is the reverse
// lookup that makes that routing work. Logged for visibility only for now (no inbox/reply feature
// built on top yet) — Meta requires a 200 within a few seconds or it will retry and eventually
// disable the subscription, so this always acks even for an unrecognized phone_number_id.
func (h *WebhookHandler) WhatsAppIncoming(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusOK) // still ack — a malformed body isn't something Meta can fix by retrying
		return
	}
	var payload waWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.log.Warn("whatsapp webhook: unparseable payload", zap.Error(err))
		w.WriteHeader(http.StatusOK)
		return
	}

	ctx := r.Context()
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			phoneNumberID := change.Value.Metadata.PhoneNumberID
			tenantID := h.resolveTenantByPhoneNumberID(ctx, phoneNumberID)

			for _, msg := range change.Value.Messages {
				h.log.Info("whatsapp incoming message",
					zap.String("tenant_id", tenantID),
					zap.String("phone_number_id", phoneNumberID),
					zap.String("from", msg.From),
					zap.String("message_id", msg.ID),
					zap.String("type", msg.Type),
				)
			}
			for _, status := range change.Value.Statuses {
				h.log.Info("whatsapp message status",
					zap.String("tenant_id", tenantID),
					zap.String("phone_number_id", phoneNumberID),
					zap.String("message_id", status.ID),
					zap.String("recipient", status.RecipientID),
					zap.String("status", status.Status),
				)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}

// resolveTenantByPhoneNumberID reverse-looks-up which tenant owns a given WhatsApp
// phone_number_id, by scanning provider_settings for a matching meta_cloud phone_number_id value.
// Returns "" (the platform's own shared number is the implicit default) when no tenant-specific
// override matches. Best-effort: any query error also falls back to "" rather than failing webhook
// processing — a lookup failure must never cause Meta to see anything but a 200.
func (h *WebhookHandler) resolveTenantByPhoneNumberID(ctx context.Context, phoneNumberID string) string {
	if phoneNumberID == "" || h.client == nil {
		return ""
	}
	row, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.ProviderType("whatsapp"),
			providersetting.ProviderName("meta_cloud"),
			providersetting.Key("phone_number_id"),
			providersetting.Value(phoneNumberID),
			providersetting.TenantIDNEQ("platform"),
		).
		First(ctx)
	if err != nil {
		return ""
	}
	return row.TenantID
}
