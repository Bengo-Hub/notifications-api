package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// subscriptionEvent is the outbox envelope from subscriptions-service.
// The shared-events library wraps the payload in an Event struct.
type subscriptionEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

// subscriptionNotificationMapping maps event types to notification details.
type subscriptionNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(payload map[string]any, tenantWebsite string) map[string]any
}

var subscriptionMappings = map[string]subscriptionNotificationMapping{
	"created": {
		TemplateID:   "subscription/subscription_created",
		EmailSubject: "Your subscription is active",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":        "Admin",
				"plan_name":   payload["plan_code"],
				"status":      payload["status"],
				"bundle_code": payload["bundle_code"],
				"trial_days":  payload["trial_days"],
				"action_link": fmt.Sprintf("%s/dashboard", tenantWebsite),
			}
		},
	},
	"upgraded": {
		TemplateID:   "subscription/subscription_upgraded",
		EmailSubject: "Your subscription has been upgraded",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":          "Admin",
				"new_plan_name": payload["new_plan_code"],
				"old_plan_name": payload["old_plan_id"],
				"action_link":   fmt.Sprintf("%s/settings/subscription", tenantWebsite),
			}
		},
	},
	"downgraded": {
		TemplateID:   "subscription/subscription_downgraded",
		EmailSubject: "Your subscription has been downgraded",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":          "Admin",
				"new_plan_name": payload["new_plan_code"],
				"old_plan_name": payload["old_plan_id"],
				"action_link":   fmt.Sprintf("%s/settings/subscription", tenantWebsite),
			}
		},
	},
	"cancelled": {
		TemplateID:   "subscription/subscription_cancelled",
		EmailSubject: "Your subscription has been cancelled",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":        "Admin",
				"plan_name":   payload["plan_code"],
				"reason":      payload["reason"],
				"action_link": fmt.Sprintf("%s/settings/subscription", tenantWebsite),
			}
		},
	},
	"renewed": {
		TemplateID:   "subscription/subscription_renewed",
		EmailSubject: "Your subscription has been renewed",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":        "Admin",
				"plan_name":   payload["plan_code"],
				"action_link": fmt.Sprintf("%s/settings/subscription", tenantWebsite),
			}
		},
	},
	"expiring": {
		TemplateID:   "subscription/subscription_expiring",
		EmailSubject: "Your subscription is expiring soon",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Admin",
				"plan_name":      payload["plan_code"],
				"expiry_date":    payload["expiry_date"],
				"renewal_amount": payload["renewal_amount"],
				"currency":       payload["currency"],
				"action_link":    fmt.Sprintf("%s/settings/subscription", tenantWebsite),
			}
		},
	},
}

// startSubscriptionConsumer subscribes to subscription.> events from the
// subscriptions-service and dispatches notification emails to tenant admins.
func startSubscriptionConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping subscription consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt subscriptionEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("subscription event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		mapping, ok := subscriptionMappings[evt.EventType]
		if !ok {
			logg.Debug("subscription event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		tenantID := evt.TenantID
		// Extract tenant_id from nested payload if top-level is empty
		if tenantID == "" {
			if tid, ok := evt.Payload["tenant_id"].(string); ok {
				tenantID = tid
			}
		}

		if tenantID == "" {
			logg.Warn("subscription event: no tenant_id, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		// Resolve tenant admin email and website
		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			logg.Error("subscription event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		if ti.ContactEmail == "" {
			logg.Warn("subscription event: tenant has no contact_email, skipping", zap.String("tenant_id", tenantID))
			_ = m.Ack()
			return
		}

		tenantWebsite := ti.Website
		if tenantWebsite == "" {
			tenantWebsite = "https://pricing.codevertexitsolutions.com"
		}

		msg := messaging.Message{
			TenantID:   tenantID,
			Channel:    "email",
			TemplateID: mapping.TemplateID,
			To:         []string{ti.ContactEmail},
			Data:       mapping.DataBuilder(evt.Payload, tenantWebsite),
			Metadata: map[string]any{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("subscription-%s-%s", evt.EventType, evt.AggregateID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("subscription event: failed to dispatch notification",
				zap.String("type", evt.EventType),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("subscription notification dispatched",
			zap.String("type", evt.EventType),
			zap.String("template", mapping.TemplateID),
			zap.String("to", ti.ContactEmail),
		)
		_ = m.Ack()
	}

	_, err := js.Subscribe("subscription.>", handler,
		nats.BindStream("subscription"),
		nats.Durable("notifications-subscription-lifecycle"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	if err != nil {
		logg.Warn("subscription consumer subscription failed (subscription stream may not exist yet)", zap.Error(err))
		return
	}

	logg.Info("subscription consumer started", zap.String("subject", "subscription.>"))
}
