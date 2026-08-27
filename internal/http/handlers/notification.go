package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	devauth "github.com/bengobox/notifications-api/internal/http/middleware"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/billing"
	"github.com/bengobox/notifications-api/internal/sandbox"
)

type NotificationHandler struct {
	log            *zap.Logger
	nats           *nats.Conn
	cache          *redis.Client
	eventsCfg      config.EventsConfig
	entClient      *ent.Client
	rateLimiter    *ratelimit.Quota
	upgradeURL     string
	billingSvc     *billing.Service
	whatsappSubSvc *billing.WhatsAppSubscriptionService
	sandboxStore   *sandbox.Store
	platformID     string
}

// SetSandboxStore wires the Redis-backed sandbox message store. Optional — a nil
// store just means sandbox-mode requests are still accepted but their "sent" history
// isn't retrievable (degrades safely if Redis isn't configured yet).
func (h *NotificationHandler) SetSandboxStore(s *sandbox.Store) { h.sandboxStore = s }

type CreateMessageRequest struct {
	Channel  string         `json:"channel" binding:"required" example:"email"`
	Tenant   string         `json:"tenant" binding:"required" example:"codevertex"`
	Template string         `json:"template" binding:"required" example:"invoice_due"`
	Data     map[string]any `json:"data" binding:"required" swaggertype:"object" example:"{\"name\":\"Jane\",\"invoice_number\":\"INV-1001\",\"amount\":\"KES 1,200\",\"due_date\":\"2025-11-30\",\"payment_link\":\"https://pay.example.com/invoices/INV-1001\",\"brand_name\":\"Codevertex\"}"`
	To       []string       `json:"to" binding:"required,min=1" example:"customer@example.com"`
	Cc       []string       `json:"cc,omitempty" example:"manager@example.com"`
	Metadata map[string]any `json:"metadata" swaggertype:"object" example:"{\"subject\":\"Invoice INV-1001 is due\",\"provider\":\"smtp\"}"`
	// Optional email attachments (base64-encoded content). Ignored for non-email channels.
	Attachments []MessageAttachmentRequest `json:"attachments,omitempty"`
}

// MessageAttachmentRequest is an optional base64 file attachment on a REST send request.
type MessageAttachmentRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	Content     string `json:"content"` // base64-encoded
}

// decodeAttachments converts base64 request attachments to messaging.Attachment,
// skipping any with empty/invalid content.
func decodeAttachments(in []MessageAttachmentRequest) []messaging.Attachment {
	var out []messaging.Attachment
	for _, a := range in {
		if a.Filename == "" || a.Content == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(a.Content)
		if err != nil {
			continue
		}
		out = append(out, messaging.Attachment{Filename: a.Filename, ContentType: a.ContentType, Content: raw})
	}
	return out
}

func NewNotificationHandler(log *zap.Logger, natsConn *nats.Conn, cache *redis.Client, eventsCfg config.EventsConfig, entClient *ent.Client, upgradeURL string, billingSvc *billing.Service, whatsappSubSvc *billing.WhatsAppSubscriptionService, platformID string) *NotificationHandler {
	var rl *ratelimit.Quota
	if cache != nil {
		rl = ratelimit.NewQuota(cache)
	}
	return &NotificationHandler{
		log:            log,
		nats:           natsConn,
		cache:          cache,
		eventsCfg:      eventsCfg,
		entClient:      entClient,
		rateLimiter:    rl,
		upgradeURL:     upgradeURL,
		billingSvc:     billingSvc,
		whatsappSubSvc: whatsappSubSvc,
		platformID:     platformID,
	}
}

// channelRateLimitKey maps notification channel to the subscription limit key.
// SMS and WhatsApp are credit-based (not rate-limited) — tenants can send as
// long as they have credit balance. Only email and webhook are rate-limited.
func channelRateLimitKey(channel string) string {
	switch channel {
	case "email":
		return "email_notifications_per_day"
	case "webhook":
		return "webhook_calls_per_day"
	default:
		// sms, whatsapp, push — not rate-limited (credit-based or subscription-gated)
		return ""
	}
}

type enqueueResponse struct {
	Status    string `json:"status" example:"queued"`
	RequestID string `json:"requestId" example:"req_123"`
}

type errorResponse struct {
	Error string `json:"error" example:"validation failed"`
}

// Enqueue receives a notification request and queues it for delivery.
// @Summary Queue notification message
// @Description Accepts a notification payload and queues it for downstream processing.
// @Tags Notifications
// @Accept json
// @Produce json
// @Param tenantId path string true "Tenant identifier"
// @Param request body CreateMessageRequest true "Message payload"
// @Example request
//
//	{
//	  "channel": "email",
//	  "tenant": codevertex,
//	  "template": "invoice_due",
//	  "to": ["customer@example.com"],
//	  "data": {
//	    "name": "Jane",
//	    "invoice_number": "INV-1001",
//	    "amount": "KES 1,200",
//	    "due_date": "2025-11-30",
//	    "payment_link": "https://pay.example.com/invoices/INV-1001",
//	    "brand_name": codevertex
//	  },
//	  "metadata": { "subject": "Invoice INV-1001 is due", "provider": "smtp" }
//	}
//
// @Success 202 {object} enqueueResponse
// @Failure 400 {object} errorResponse
// @Security bearerAuth
// @Security ApiKeyAuth
// @Router /{tenantId}/notifications/messages [post]
func (h *NotificationHandler) Enqueue(w http.ResponseWriter, r *http.Request) {
	var req CreateMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: err.Error()})
		return
	}

	tenant := req.Tenant
	if tenant == "" {
		tenant = resolveActingTenantID(r)
	}
	if tenant == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "tenant required"})
		return
	}

	// Normalize recipients: split any comma/semicolon/newline-joined elements into
	// individual validated addresses so no address is dropped (and SMTP doesn't 501
	// on a joined element). Runs before rate-limiting so the recipient count is accurate.
	req.To = messaging.NormalizeRecipients(req.To, req.Channel)
	req.Cc = messaging.NormalizeRecipients(req.Cc, req.Channel)
	if req.Channel == "email" && len(req.To) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "no valid recipient addresses"})
		return
	}

	// Sandbox short-circuit: a sandbox App token never reaches a real provider, never
	// touches the real DeliveryLog table, never counts against real billing/usage —
	// it's saved to the ephemeral sandbox store instead and reported back identically
	// to a real "queued" response, so integration code doesn't need an if/else for
	// sandbox vs production. Skips every guard below (rate limit, credit balance,
	// NATS publish, usage event, DeliveryLog) entirely on purpose.
	if devauth.EnvironmentFromContext(r.Context()) == "sandbox" {
		h.enqueueSandbox(w, r, tenant, req)
		return
	}

	// Per-channel rate limiting based on subscription plan
	if h.rateLimiter != nil {
		limitKey := channelRateLimitKey(req.Channel)
		if limitKey != "" {
			claims, _ := authclient.ClaimsFromContext(r.Context())
			if claims != nil {
				limit := claims.GetLimit(limitKey)
				if limit != 0 {
					// Multiply limit check by number of recipients
					for range req.To {
						result, _ := h.rateLimiter.Check(r.Context(), tenant, limitKey, limit)
						if result != nil && !result.Allowed {
							w.Header().Set("Content-Type", "application/json")
							w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", result.Limit))
							w.Header().Set("X-RateLimit-Remaining", "0")
							w.Header().Set("X-RateLimit-Feature", limitKey)
							w.Header().Set("Retry-After", "86400")
							w.WriteHeader(http.StatusTooManyRequests)
							json.NewEncoder(w).Encode(map[string]any{
								"error":       "usage_limit_reached",
								"feature":     limitKey,
								"limit":       result.Limit,
								"used":        result.Used,
								"upgrade_url": h.upgradeURL,
								"message":     fmt.Sprintf("Daily %s limit reached. Upgrade your plan or add overage.", limitKey),
							})
							return
						}
					}
				}
			}
		}
	}

	// Pre-send credit guard for SMS — reject if balance is insufficient.
	if h.billingSvc != nil && req.Channel == "sms" {
		if tid, err := uuid.Parse(tenant); err == nil {
			balance, balErr := h.billingSvc.GetBalance(r.Context(), tid, "SMS")
			if balErr != nil {
				h.log.Warn("sms credit balance check failed", zap.Error(balErr), zap.String("tenant", tenant))
			} else if balance <= 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPaymentRequired)
				json.NewEncoder(w).Encode(map[string]any{
					"error":     "insufficient_credits",
					"type":      "SMS",
					"balance":   balance,
					"message":   "Insufficient SMS credits. Top up your balance to continue sending.",
					"topup_url": h.upgradeURL,
				})
				return
			}
		}
	}

	// Pre-send WhatsApp guard — tenant must have active subscription + quota remaining. The
	// platform tenant itself is exempt (see cmd/worker's identical exemption for the real gate);
	// this is just the early synchronous pre-check for HTTP-enqueued sends.
	if req.Channel == "whatsapp" && tenant != h.platformID {
		if tid, err := uuid.Parse(tenant); err == nil {
			if h.whatsappSubSvc != nil {
				// Read-only pre-check for the synchronous 402 response. The actual quota counter
				// increment happens once on the delivery path (worker CheckQuota) — using the
				// mutating CheckQuota here too would double-count this single message.
				if quotaErr := h.whatsappSubSvc.VerifyQuota(r.Context(), tid); quotaErr != nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusPaymentRequired)
					errCode := "no_active_subscription"
					if len(quotaErr.Error()) > 15 && quotaErr.Error()[:15] == "quota_exhausted" {
						errCode = "quota_exhausted"
					}
					json.NewEncoder(w).Encode(map[string]any{
						"error":     errCode,
						"message":   quotaErr.Error(),
						"topup_url": h.upgradeURL,
					})
					return
				}
			}
		}
	}

	requestID := httpware.GetRequestID(r.Context())
	idemp := r.Header.Get("Idempotency-Key")
	if idemp == "" {
		// derive from payload
		sum := sha256.Sum256([]byte(tenant + "|" + req.Channel + "|" + req.Template + "|" + requestID))
		idemp = hex.EncodeToString(sum[:])
	}

	// idempotency check (24h)
	if h.cache != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
		defer cancel()
		key := "idemp:" + idemp
		ok, err := h.cache.SetNX(ctx, key, requestID, 24*time.Hour).Result()
		if err != nil {
			h.log.Warn("idempotency setnx failed", zap.Error(err))
		}
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(enqueueResponse{Status: "duplicate", RequestID: requestID})
			return
		}
	}

	msg := messaging.Message{
		TenantID:       tenant,
		Channel:        req.Channel,
		TemplateID:     req.Template,
		Data:           req.Data,
		To:             req.To,
		Cc:             req.Cc,
		Metadata:       req.Metadata,
		Attachments:    decodeAttachments(req.Attachments),
		RequestID:      requestID,
		IdempotencyKey: idemp,
		QueuedAt:       time.Now(),
	}

	if _, err := messaging.Publish(r.Context(), h.nats, h.eventsCfg, msg); err != nil {
		h.log.Error("publish failed", zap.Error(err), zap.String("request_id", requestID))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(errorResponse{Error: "queue_unavailable"})
		return
	}

	// Publish per-channel usage event for subscriptions-api limit tracking.
	h.publishUsageEvent(r.Context(), tenant, req.Channel)

	recordDeliveryLog(r.Context(), h.entClient, tenant, req.Template, req.Channel, req.To)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(enqueueResponse{Status: "queued", RequestID: requestID})
}

// ListSandboxMessages handles GET /api/v1/sandbox/messages — lets a developer see what
// their integration has "sent" while testing in sandbox mode. Scoped to the caller's own
// tenant (resolved the same way every other route resolves it); a caller who has never
// sent a sandbox message just gets an empty list, including any caller using a
// production key, since sandbox sends are never stored under a production context.
func (h *NotificationHandler) ListSandboxMessages(w http.ResponseWriter, r *http.Request) {
	tenant := resolveActingTenantID(r)
	if tenant == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "tenant required"})
		return
	}
	messages, err := h.sandboxStore.List(r.Context(), tenant)
	if err != nil {
		h.log.Warn("list sandbox messages failed", zap.Error(err), zap.String("tenant", tenant))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(errorResponse{Error: "failed to list sandbox messages"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"data": messages, "count": len(messages)})
}

// enqueueSandbox simulates sending req without ever touching a real provider, the
// real DeliveryLog table, or real billing/usage counters. Saved to the ephemeral
// Redis-backed sandbox store (see internal/sandbox) so the developer can inspect what
// they "sent" via GET /api/v1/sandbox/messages.
func (h *NotificationHandler) enqueueSandbox(w http.ResponseWriter, r *http.Request, tenant string, req CreateMessageRequest) {
	requestID := httpware.GetRequestID(r.Context())
	if requestID == "" {
		requestID = "sandbox_" + uuid.NewString()
	}

	if h.sandboxStore != nil {
		msg := sandbox.Message{
			ID:            requestID,
			Channel:       req.Channel,
			Template:      req.Template,
			To:            req.To,
			Data:          req.Data,
			Status:        "sandbox_simulated",
			SentAt:        time.Now(),
			SimulatedNote: "Sandbox mode — no real " + req.Channel + " was sent. This entry expires automatically.",
		}
		if err := h.sandboxStore.Save(r.Context(), tenant, msg); err != nil {
			h.log.Warn("sandbox store save failed", zap.Error(err), zap.String("tenant", tenant))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(enqueueResponse{Status: "queued", RequestID: requestID})
}

// publishUsageEvent publishes a notifications.<channel>.sent event to NATS for subscriptions-api
// usage tracking. Non-fatal — errors are logged and ignored.
func (h *NotificationHandler) publishUsageEvent(ctx context.Context, tenant, channel string) {
	if h.nats == nil {
		return
	}
	subject := ""
	switch channel {
	case "email":
		subject = "notifications.email.sent"
	case "sms":
		subject = "notifications.sms.sent"
	case "push":
		subject = "notifications.push.sent"
	default:
		return
	}

	// Shared-events envelope + event-id header: the subscriptions usage consumer dedupes
	// via EventIDFromMsg, so a bespoke flat payload without an id made redeliveries
	// double-meter. tenant may be a slug for some callers — only a UUID goes in tenant_id.
	tenantUUID := uuid.Nil
	if id, err := uuid.Parse(tenant); err == nil {
		tenantUUID = id
	}
	ev := eventslib.NewEvent(channel+".sent", "notifications", uuid.New(), tenantUUID, map[string]any{
		"tenant_id": tenant,
		"channel":   channel,
	})
	payload, err := ev.ToJSON()
	if err != nil {
		h.log.Warn("usage event: marshal envelope failed", zap.Error(err))
		return
	}
	js, err := h.nats.JetStream()
	if err != nil {
		h.log.Warn("usage event: jetstream init failed", zap.Error(err))
		return
	}
	msg := nats.NewMsg(subject)
	msg.Data = payload
	msg.Header = nats.Header{}
	msg.Header.Set("event-id", ev.ID.String())
	if _, err := js.PublishMsg(msg); err != nil {
		h.log.Warn("usage event: publish failed", zap.String("subject", subject), zap.Error(err))
	}
}

// EnqueueMessage enqueues a notification message (used by template test-send and other callers).
// Returns requestID and error. If err != nil, the message was not queued.
func (h *NotificationHandler) EnqueueMessage(ctx context.Context, tenantID, channel, templateID string, to []string, data, metadata map[string]any) (requestID string, err error) {
	if tenantID == "" || channel == "" || templateID == "" || len(to) == 0 {
		return "", fmt.Errorf("tenant, channel, template and to required")
	}
	rid := httpware.GetRequestID(ctx)
	if rid == "" {
		rid = fmt.Sprintf("test_%d", time.Now().UnixNano())
	}
	idemp := fmt.Sprintf("test:%s:%s:%s:%s", tenantID, channel, templateID, rid)
	if h.cache != nil {
		cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		ok, _ := h.cache.SetNX(cctx, "idemp:"+idemp, rid, 24*time.Hour).Result()
		if !ok {
			return rid, nil // duplicate, treat as success
		}
	}
	msg := messaging.Message{
		TenantID:       tenantID,
		Channel:        channel,
		TemplateID:     templateID,
		Data:           data,
		To:             to,
		Metadata:       metadata,
		RequestID:      rid,
		IdempotencyKey: idemp,
		QueuedAt:       time.Now(),
	}
	if _, err := messaging.Publish(ctx, h.nats, h.eventsCfg, msg); err != nil {
		return "", err
	}
	recordDeliveryLog(ctx, h.entClient, tenantID, templateID, channel, to)
	return rid, nil
}

func recordDeliveryLog(ctx context.Context, client *ent.Client, tenantID, templateID, channel string, to []string) {
	if client == nil || len(to) == 0 {
		return
	}
	for _, recipient := range to {
		_, err := client.DeliveryLog.Create().
			SetTenantID(tenantID).
			SetTemplateID(templateID).
			SetChannel(channel).
			SetRecipient(recipient).
			SetStatus("sent").
			Save(ctx)
		if err != nil {
			// best-effort; do not fail the request
			return
		}
	}
}
