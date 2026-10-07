package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
	"github.com/bengobox/notifications-api/internal/modules/whatsappinbox"

	"github.com/Bengo-Hub/httpware/pii"
)

// whatsAppFallback queues the backup number of a WhatsApp message Meta could not deliver.
type whatsAppFallback struct {
	rdb    redis.UniversalClient
	nc     *nats.Conn
	events config.EventsConfig
}

// WithWhatsAppFallback lets status webhooks move an undelivered message to its next number
// (messaging.ParkWhatsAppFallback). Without it, failures are only logged.
func (h *WebhookHandler) WithWhatsAppFallback(rdb redis.UniversalClient, nc *nats.Conn, events config.EventsConfig) *WebhookHandler {
	if rdb != nil && nc != nil {
		h.fallback = &whatsAppFallback{rdb: rdb, nc: nc, events: events}
	}
	return h
}

// onWhatsAppStatus acts on a delivery status for the backup-number flow: failed sends the parked
// next attempt, delivered or read forgets it.
func (h *WebhookHandler) onWhatsAppStatus(ctx context.Context, messageID, status string) {
	if h.fallback == nil || messageID == "" {
		return
	}
	switch status {
	case "failed":
		next, ok := messaging.TakeWhatsAppFallback(ctx, h.fallback.rdb, messageID)
		if !ok {
			return
		}
		if _, err := messaging.Publish(ctx, h.fallback.nc, h.fallback.events, *next); err != nil {
			h.log.Warn("whatsapp: could not queue the backup number", zap.String("message_id", messageID), zap.Error(err))
			return
		}
		h.log.Info("whatsapp: undelivered, sent to the backup number", zap.String("message_id", messageID))
	case "delivered", "read":
		messaging.DropWhatsAppFallback(ctx, h.fallback.rdb, messageID)
	}
}

// WebhookHandler receives provider-initiated callbacks (SMS delivery reports, WhatsApp message/
// status webhooks) — all public, unauthenticated routes, matching treasury-api's
// /webhooks/{provider}/... convention for the same reason: the provider calls these directly with
// no tenant JWT to attach.
type WebhookHandler struct {
	client        *ent.Client
	log           *zap.Logger
	publicBaseURL string
	inbox         *whatsappinbox.Service
	fallback      *whatsAppFallback
	optOut        *suppression.Service
}

// NewWebhookHandler creates the webhook handler. publicBaseURL is this service's own externally
// reachable base URL, used to compose the callback URLs shown to admins (see Config). inbox is
// optional (nil-safe) — when unset, inbound WhatsApp messages are still logged but not persisted.
func NewWebhookHandler(client *ent.Client, log *zap.Logger, publicBaseURL string, inbox *whatsappinbox.Service) *WebhookHandler {
	return &WebhookHandler{client: client, log: log.Named("webhooks"), publicBaseURL: publicBaseURL, inbox: inbox}
}

// Config returns the provider-facing callback URLs and the WhatsApp verify token, so a tenant or
// platform admin can copy-paste them directly into Meta's WhatsApp Manager / Africa's Talking
// dashboard instead of having to know or guess these values. Not a secret in the credential sense
// (it's a handshake string Meta echoes back during webhook verification, not an access token) but
// still gated behind auth like the rest of Settings, matching GetSecuritySettings.
func (h *WebhookHandler) Config(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]string{
		"whatsapp_callback_url":           h.publicBaseURL + "/api/v1/webhooks/whatsapp/meta",
		"whatsapp_verify_token":           metaWebhookVerifyToken(),
		"africastalking_dlr_callback_url": h.publicBaseURL + "/api/v1/webhooks/africastalking/dlr",
		// Incoming messages: STOP replies opt the number out of marketing SMS.
		"africastalking_inbound_callback_url": h.publicBaseURL + "/api/v1/webhooks/africastalking/inbound",
	})
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
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
					// A template quick-reply tap ("Stop promotions") arrives as type "button".
					Button struct {
						Payload string `json:"payload"`
						Text    string `json:"text"`
					} `json:"button"`
				} `json:"messages"`
				Statuses []struct {
					ID          string `json:"id"`
					Status      string `json:"status"` // sent | delivered | read | failed
					RecipientID string `json:"recipient_id"`
					Errors      []struct {
						Code      int    `json:"code"`
						Title     string `json:"title"`
						Message   string `json:"message"`
						ErrorData struct {
							Details string `json:"details"`
						} `json:"error_data"`
					} `json:"errors"`
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

			contactNames := make(map[string]string, len(change.Value.Contacts))
			for _, c := range change.Value.Contacts {
				contactNames[c.WaID] = c.Profile.Name
			}

			for _, msg := range change.Value.Messages {
				h.log.Info("whatsapp incoming message",
					zap.String("tenant_id", tenantID),
					zap.String("phone_number_id", phoneNumberID),
					pii.Phone("from", msg.From),
					zap.String("message_id", msg.ID),
					zap.String("type", msg.Type),
				)
				// "Stop promotions" button or a STOP text: opt this number out of marketing.
				stopText := msg.Text.Body
				if msg.Type == "button" {
					stopText = msg.Button.Text
				}
				h.handleStopReply(ctx, "whatsapp", msg.From, stopText, tenantID, "whatsapp_reply")
				if h.inbox != nil && tenantID != "" {
					if tid, err := uuid.Parse(tenantID); err == nil {
						if err := h.inbox.RecordInbound(ctx, tid, phoneNumberID, msg.ID, msg.From, msg.Type, msg.Text.Body, contactNames[msg.From]); err != nil {
							h.log.Warn("failed to persist inbound whatsapp message", zap.Error(err), zap.String("message_id", msg.ID))
						}
					}
				}
			}
			for _, status := range change.Value.Statuses {
				h.log.Info("whatsapp message status",
					zap.String("tenant_id", tenantID),
					zap.String("phone_number_id", phoneNumberID),
					zap.String("message_id", status.ID),
					zap.String("recipient", status.RecipientID),
					zap.String("status", status.Status),
				)
				// A "failed" status carries the actual reason in Meta's own errors array (e.g.
				// "Recipient phone number not in allowed list" for a test/dev app, or a 24-hour
				// session-window violation) -- previously discarded entirely, so a failed send
				// showed only the fact of failure, never why.
				for _, e := range status.Errors {
					h.log.Warn("whatsapp message failed",
						zap.String("message_id", status.ID),
						zap.String("recipient", status.RecipientID),
						zap.Int("error_code", e.Code),
						zap.String("error_title", e.Title),
						zap.String("error_message", e.Message),
						zap.String("error_details", e.ErrorData.Details),
					)
				}
				h.onWhatsAppStatus(ctx, status.ID, status.Status)
				if h.inbox != nil {
					if err := h.inbox.RecordStatusUpdate(ctx, status.ID, status.Status); err != nil {
						h.log.Warn("failed to record whatsapp status update", zap.Error(err), zap.String("message_id", status.ID))
					}
				}
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
