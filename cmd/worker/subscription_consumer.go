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
	// invoice_generated fires when subscriptions issues a subscription invoice (7 days
	// before expiry or via manual platform-owner generation). Emails the tenant the
	// invoice with a durable pay link + PDF. Reuses the finance/invoice_sent template.
	"invoice_generated": {
		TemplateID:   "finance/invoice_sent",
		EmailSubject: "Your subscription invoice is ready",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Admin",
				"amount":         fmt.Sprintf("%v %v", payload["currency"], payload["amount"]),
				"due_date":       formatEventDate(payload["due_date"]),
				"invoice_number": payload["invoice_number"],
				"invoice_link":   payload["pdf_url"], // public branded PDF
				"payment_link":   payload["pay_url"], // durable treasury pay page
			}
		},
	},
	// grace_reminder fires once per day during the post-expiry grace period with a
	// decremented countdown. payload.pay_link is the durable treasury pay page.
	"grace_reminder": {
		TemplateID:   "subscription/grace_reminder",
		EmailSubject: "Action required: pay to keep your services active",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Admin",
				"plan_name":      payload["plan_code"],
				"days_remaining": payload["days_remaining"],
				"grace_ends_at":  payload["grace_ends_at"],
				"amount":         payload["amount"],
				"invoice_number": payload["invoice_number"],
				"payment_link":   firstNonEmpty(payload["pay_link"], fmt.Sprintf("%s/settings/subscription", tenantWebsite)),
			}
		},
	},
	// grace_started fires once when a subscription enters grace (period end passed unpaid).
	"grace_started": {
		TemplateID:   "subscription/grace_reminder",
		EmailSubject: "Your subscription payment is overdue",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Admin",
				"plan_name":      payload["plan_code"],
				"days_remaining": payload["days_remaining"],
				"grace_ends_at":  payload["grace_ends_at"],
				"amount":         payload["amount"],
				"invoice_number": payload["invoice_number"],
				"payment_link":   firstNonEmpty(payload["pay_link"], fmt.Sprintf("%s/settings/subscription", tenantWebsite)),
			}
		},
	},
}

// firstNonEmpty returns the first argument that is a non-empty string, else "".
func firstNonEmpty(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
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

		// Check for explicit recipient in event payload notification block
		recipientEmail := ti.ContactEmail
		if notif, ok := evt.Payload["notification"].(map[string]any); ok {
			if re, ok := notif["recipient_email"].(string); ok && re != "" {
				recipientEmail = re
			}
		}

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopePlatform,
			Target:      messaging.TargetTenantAdmin,
			To:          []string{recipientEmail},
			Data:        mapping.DataBuilder(evt.Payload, tenantWebsite),
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

	subscribeWithRetry(ctx, js, logg, "subscription consumer", true, func() (*nats.Subscription, error) {
		return js.Subscribe("subscription.>", handler,
			nats.BindStream("subscription"),
			nats.Durable("notifications-subscription-lifecycle"),
			nats.ManualAck(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(3),
		)
	})
}
