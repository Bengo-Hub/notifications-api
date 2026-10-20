package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/maildelivery"
)

// mailDeliveryLastEventKey remembers when the mail server last called the webhook, so the Platform
// page can show whether feedback is flowing.
const mailDeliveryLastEventKey = "notifications:maildelivery:last-event"

// maxMailWebhookBody bounds one webhook batch; Stalwart batches events per second, never megabytes.
const maxMailWebhookBody = 1 << 20

// MailDeliveryHandler receives the mail server's delivery events and lets a platform admin rotate
// the shared signing key.
type MailDeliveryHandler struct {
	svc        *maildelivery.Service
	keys       *maildelivery.Keys
	apply      maildelivery.Applier
	rdb        redis.UniversalClient
	webhookURL string
	log        *zap.Logger
}

// NewMailDeliveryHandler wires the handler. rdb may be nil (the last-event time is then not tracked).
func NewMailDeliveryHandler(svc *maildelivery.Service, keys *maildelivery.Keys, apply maildelivery.Applier,
	rdb redis.UniversalClient, webhookURL string, log *zap.Logger) *MailDeliveryHandler {
	return &MailDeliveryHandler{svc: svc, keys: keys, apply: apply, rdb: rdb, webhookURL: webhookURL, log: log}
}

// NewMailDeliverySuppressor stores a hard-bounced address in the shared Redis suppression list the
// worker's email guard reads before every send.
func NewMailDeliverySuppressor(rdb redis.UniversalClient) maildelivery.Suppressor {
	return func(ctx context.Context, addr string, ttl time.Duration) {
		messaging.SuppressEmail(ctx, rdb, addr, ttl)
	}
}

// Webhook is called by the mail server. POST /api/v1/webhooks/stalwart/delivery
// It is public like the other provider webhooks, so the HMAC signature is the only authentication:
// an unsigned or wrongly signed call is refused before anything is parsed.
func (h *MailDeliveryHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMailWebhookBody))
	if err != nil {
		http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	keys, err := h.keys.Active(r.Context())
	if err != nil {
		h.log.Error("mail delivery: could not read signing keys", zap.Error(err))
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if len(keys) == 0 {
		http.Error(w, "webhook is not configured", http.StatusServiceUnavailable)
		return
	}
	if !maildelivery.Verify(body, r.Header.Get("X-Signature"), keys...) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	events, err := maildelivery.ParseEvents(body)
	if err != nil {
		http.Error(w, "invalid event batch", http.StatusBadRequest)
		return
	}
	res := h.svc.Handle(r.Context(), events)
	if h.rdb != nil {
		h.rdb.Set(r.Context(), mailDeliveryLastEventKey, time.Now().UTC().Format(time.RFC3339), 30*24*time.Hour)
	}
	h.log.Debug("mail delivery webhook", zap.Int("events", res.Events), zap.Int("bounced", res.Bounced), zap.Int("suppressed", res.Suppressed))
	w.WriteHeader(http.StatusNoContent)
}

// mailDeliveryStatus has the same shape as the encryption key status (configured, source,
// key_fingerprint, updated_at) so the Platform UI renders both with one card, plus the webhook details.
type mailDeliveryStatus struct {
	Configured     bool       `json:"configured"`
	Source         string     `json:"source"`
	KeyFingerprint string     `json:"key_fingerprint"`
	UpdatedAt      *time.Time `json:"updated_at"`
	LastEventAt    *time.Time `json:"last_event_at,omitempty"`
	WebhookURL     string     `json:"webhook_url"`
}

// Status reports whether delivery feedback is set up, never the key itself.
// GET /api/v1/platform/mail-delivery-key
func (h *MailDeliveryHandler) Status(w http.ResponseWriter, r *http.Request) {
	st := mailDeliveryStatus{WebhookURL: h.webhookURL}
	if ok, at := h.keys.Configured(r.Context()); ok {
		st.Configured = true
		st.Source = "db"
		st.KeyFingerprint = h.keys.Fingerprint(r.Context())
		st.UpdatedAt = &at
	}
	if h.rdb != nil {
		if v, err := h.rdb.Get(r.Context(), mailDeliveryLastEventKey).Result(); err == nil {
			if t, perr := time.Parse(time.RFC3339, v); perr == nil {
				st.LastEventAt = &t
			}
		}
	}
	notifRespondJSON(w, http.StatusOK, st)
}

// Rotate creates a new signing key and applies it to the mail server, creating the webhook on the
// first call. The old key stays valid so batches already in flight are accepted. Like the encryption
// key it takes {"generate": true}; a hand-supplied key is not offered because the mail server must
// receive the same value and only a generated one is pushed there.
// PUT /api/v1/platform/mail-delivery-key
func (h *MailDeliveryHandler) Rotate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Generate bool `json:"generate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !req.Generate {
		notifRespondJSON(w, http.StatusBadRequest, map[string]string{"error": `send {"generate": true} to rotate the key`})
		return
	}
	if _, err := h.keys.Rotate(r.Context(), h.apply); err != nil {
		h.log.Error("mail delivery: key rotation failed", zap.Error(err))
		notifRespondJSON(w, http.StatusBadGateway, map[string]string{"error": "the mail server did not accept the new key, nothing was changed"})
		return
	}
	h.log.Info("mail delivery: webhook key rotated")
	h.Status(w, r)
}

// RegisterPlatformRoutes mounts the platform admin routes, next to the encryption key routes.
func (h *MailDeliveryHandler) RegisterPlatformRoutes(r chi.Router) {
	r.Get("/mail-delivery-key", h.Status)
	r.Put("/mail-delivery-key", h.Rotate)
}
