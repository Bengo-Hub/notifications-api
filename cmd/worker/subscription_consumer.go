package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/billing"
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

// fulfillCustomAddon handles a subscriptions-api CustomAddon activation that needs provisioning
// inside THIS service — service_addon_type "sms_bundle" grants SMS credits, "whatsapp_plan"
// activates a WhatsApp subscription plan. Payload shape (set by subscriptions-api's
// CustomAddonHandler): {tenant_id, service_code, service_addon_type, quantity, metadata}, where
// metadata carries {"sms_credits": N} or {"whatsapp_plan_id": "<uuid>"}. Best-effort: any
// malformed/unrecognized payload just logs and returns — this must never crash the worker over an
// admin data-entry mistake on the subscriptions-api side.
func fulfillCustomAddon(ctx context.Context, evt subscriptionEvent, billingSvc *billing.Service, whatsappSubsSvc *billing.WhatsAppSubscriptionService, logg *zap.Logger) {
	tenantIDStr := evt.TenantID
	if tenantIDStr == "" {
		if tid, ok := evt.Payload["tenant_id"].(string); ok {
			tenantIDStr = tid
		}
	}
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		logg.Warn("custom_addon.activated: invalid/missing tenant_id, skipping", zap.String("raw", tenantIDStr))
		return
	}

	serviceAddonType, _ := evt.Payload["service_addon_type"].(string)
	metadata, _ := evt.Payload["metadata"].(map[string]any)

	switch serviceAddonType {
	case "sms_bundle":
		if billingSvc == nil {
			logg.Warn("custom_addon.activated: sms_bundle received but billing service unavailable")
			return
		}
		credits, ok := metadata["sms_credits"].(float64)
		if !ok || credits <= 0 {
			logg.Warn("custom_addon.activated: sms_bundle missing/invalid sms_credits in metadata",
				zap.String("tenant_id", tenantIDStr))
			return
		}
		if err := billingSvc.TopUpCredits(ctx, tenantID, "SMS", credits, "addon-"+evt.AggregateID); err != nil {
			logg.Error("custom_addon.activated: sms credit grant failed",
				zap.String("tenant_id", tenantIDStr), zap.Error(err))
			return
		}
		logg.Info("custom_addon.activated: sms credits granted",
			zap.String("tenant_id", tenantIDStr), zap.Float64("credits", credits))

	case "whatsapp_plan":
		if whatsappSubsSvc == nil {
			logg.Warn("custom_addon.activated: whatsapp_plan received but whatsapp subscription service unavailable")
			return
		}
		planIDStr, _ := metadata["whatsapp_plan_id"].(string)
		planID, perr := uuid.Parse(planIDStr)
		if perr != nil {
			logg.Warn("custom_addon.activated: whatsapp_plan missing/invalid whatsapp_plan_id in metadata",
				zap.String("tenant_id", tenantIDStr), zap.String("raw", planIDStr))
			return
		}
		if err := whatsappSubsSvc.ActivateSubscription(ctx, tenantID, planID, "addon-"+evt.AggregateID); err != nil {
			logg.Error("custom_addon.activated: whatsapp plan activation failed",
				zap.String("tenant_id", tenantIDStr), zap.Error(err))
			return
		}
		logg.Info("custom_addon.activated: whatsapp plan activated",
			zap.String("tenant_id", tenantIDStr), zap.String("plan_id", planIDStr))

	default:
		logg.Debug("custom_addon.activated: unhandled service_addon_type, skipping",
			zap.String("service_addon_type", serviceAddonType))
	}
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
// billingSvc/whatsappSubsSvc fulfill "custom_addon.activated" events (a tenant's admin attached
// an SMS-credit or WhatsApp-plan addon to their platform subscription in subscriptions-ui) —
// reuses the exact same in-process call shapes treasury_consumer.go already uses for real payment
// top-ups/activations, on the "subscription" stream this consumer already binds to, so this needs
// no new HTTP route or auth surface between the two services. Either may be nil (fulfillment then
// just logs and skips, matching every other best-effort branch in this worker).
func startSubscriptionConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, billingSvc *billing.Service, whatsappSubsSvc *billing.WhatsAppSubscriptionService, logg *zap.Logger) {
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

		if evt.EventType == "custom_addon.activated" {
			fulfillCustomAddon(ctx, evt, billingSvc, whatsappSubsSvc, logg)
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
			tenantWebsite = "https://pricing.codevertexafrica.com"
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

	eventslib.SubscribeQueueWithRebind(logg, js, "subscription", "subscription.>", "notifications-subscription-lifecycle", handler,
		nats.BindStream("subscription"),
		nats.Durable("notifications-subscription-lifecycle"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
