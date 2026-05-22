package handlers

import (
	"context"
	"crypto/sha256"
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

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/billing"
	appmw "github.com/bengobox/notifications-api/internal/shared/middleware"
)

type NotificationHandler struct {
	log         *zap.Logger
	nats        *nats.Conn
	cache       *redis.Client
	eventsCfg   config.EventsConfig
	entClient   *ent.Client
	rateLimiter *appmw.RateLimiter
	upgradeURL  string
	billingSvc  *billing.Service
}

type CreateMessageRequest struct {
	Channel  string         `json:"channel" binding:"required" example:"email"`
	Tenant   string         `json:"tenant" binding:"required" example:codevertex`
	Template string         `json:"template" binding:"required" example:"invoice_due"`
	Data     map[string]any `json:"data" binding:"required" swaggertype:"object" example:"{\"name\":\"Jane\",\"invoice_number\":\"INV-1001\",\"amount\":\"KES 1,200\",\"due_date\":\"2025-11-30\",\"payment_link\":\"https://pay.example.com/invoices/INV-1001\",\"brand_name\":\"BengoBox\"}"`
	To       []string       `json:"to" binding:"required,min=1" example:"customer@example.com"`
	Metadata map[string]any `json:"metadata" swaggertype:"object" example:"{\"subject\":\"Invoice INV-1001 is due\",\"provider\":\"smtp\"}"`
}

func NewNotificationHandler(log *zap.Logger, natsConn *nats.Conn, cache *redis.Client, eventsCfg config.EventsConfig, entClient *ent.Client, upgradeURL string, billingSvc *billing.Service) *NotificationHandler {
	var rl *appmw.RateLimiter
	if cache != nil {
		rl = appmw.NewRateLimiter(cache)
	}
	return &NotificationHandler{
		log:         log,
		nats:        natsConn,
		cache:       cache,
		eventsCfg:   eventsCfg,
		entClient:   entClient,
		rateLimiter: rl,
		upgradeURL:  upgradeURL,
		billingSvc:  billingSvc,
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
		tenant = httpware.GetTenantID(r.Context())
	}
	if tenant == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "tenant required"})
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

	// Pre-send credit guard for SMS/WhatsApp — reject if balance is insufficient.
	// Email is rate-limited by plan quota, not credits.
	if h.billingSvc != nil && (req.Channel == "sms" || req.Channel == "whatsapp") {
		creditType := "SMS"
		if req.Channel == "whatsapp" {
			creditType = "WHATSAPP"
		}
		if tid, err := uuid.Parse(tenant); err == nil {
			balance, balErr := h.billingSvc.GetBalance(r.Context(), tid, creditType)
			if balErr != nil {
				h.log.Warn("credit balance check failed", zap.Error(balErr), zap.String("tenant", tenant))
			} else if balance <= 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusPaymentRequired)
				json.NewEncoder(w).Encode(map[string]any{
					"error":       "insufficient_credits",
					"credit_type": creditType,
					"balance":     balance,
					"message":     "Insufficient " + creditType + " credits. Top up your balance to continue sending.",
					"topup_url":   h.upgradeURL,
				})
				return
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
		Metadata:       req.Metadata,
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

	recordDeliveryLog(r.Context(), h.entClient, tenant, req.Template, req.Channel, req.To)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(enqueueResponse{Status: "queued", RequestID: requestID})
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
