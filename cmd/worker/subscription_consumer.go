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
				"new_plan_name": firstNonEmpty(payload["new_plan_name"], payload["new_plan_code"]),
				"old_plan_name": firstNonEmpty(payload["old_plan_name"], payload["old_plan_code"], payload["old_plan_id"]),
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
				"new_plan_name": firstNonEmpty(payload["new_plan_name"], payload["new_plan_code"]),
				"old_plan_name": firstNonEmpty(payload["old_plan_name"], payload["old_plan_code"], payload["old_plan_id"]),
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
				"plan_name":   firstNonEmpty(payload["plan_name"], payload["plan_code"]),
				"action_link": fmt.Sprintf("%s/settings/subscription", tenantWebsite),
			}
		},
	},
	// Upgrade trials and scheduled plan changes (subscriptions-api plan_schedule*.go). A trial
	// gives a higher tier for some days; the upgrade then takes effect on the next renewal date
	// and that renewal bills it. manage_url is the subscriptions billing page.
	"upgrade_trial_started": {
		TemplateID:   "subscription/upgrade_trial_started",
		EmailSubject: "Your free trial has started",
		DataBuilder:  upgradeTrialData,
	},
	"upgrade_trial_ending": {
		TemplateID:   "subscription/upgrade_trial_ending",
		EmailSubject: "Your free trial ends soon",
		DataBuilder:  upgradeTrialData,
	},
	"upgrade_trial_ended": {
		TemplateID:   "subscription/upgrade_trial_ended",
		EmailSubject: "Your free trial has ended",
		DataBuilder:  upgradeTrialData,
	},
	"plan_change_scheduled": {
		TemplateID:   "subscription/plan_change_scheduled",
		EmailSubject: "Your plan change is scheduled",
		DataBuilder:  upgradeTrialData,
	},
	// trial_ended fires when a signup trial on a paid plan ends unpaid and enters grace.
	"trial_ended": {
		TemplateID:   "subscription/grace_reminder",
		EmailSubject: "Your free trial has ended: pay to keep your services active",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Admin",
				"plan_name":      firstNonEmpty(payload["plan_name"], payload["plan_code"]),
				"days_remaining": payload["days_remaining"],
				"grace_ends_at":  payload["grace_ends_at"],
				"amount":         payload["amount"],
				"invoice_number": payload["invoice_number"],
				"payment_link":   firstNonEmpty(payload["pay_link"], fmt.Sprintf("%s/settings/subscription", tenantWebsite)),
			}
		},
	},
	// "expiring" removed — subscriptions-api never publishes it (grep-verified), and the
	// pre-expiry warning it would have sent is already superseded by invoice_generated (which
	// fires 7 days before expiry with a real invoice + pay link, not a second redundant notice).
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
	// Support agreements (subscriptions-api support_fee.go / support_fee_invoice.go): the standard
	// hosting and support fee of one-time-license tenants and any special support agreement. An
	// unpaid charge blocks create/edit/delete fleet-wide once its grace window ends, so these use
	// the same invoice and grace templates as subscription billing.
	"support_fee_invoice_generated": {
		TemplateID:   "finance/invoice_sent",
		EmailSubject: "Your support invoice is ready",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Admin",
				"amount":         fmt.Sprintf("%v %v", payload["currency"], payload["amount"]),
				"due_date":       formatEventDate(payload["due_date"]),
				"invoice_number": payload["invoice_number"],
				"invoice_link":   payload["pdf_url"],
				"payment_link":   payload["pay_url"],
			}
		},
	},
	"support_fee_overdue": {
		TemplateID:   "subscription/grace_reminder",
		EmailSubject: "Your support payment is overdue",
		DataBuilder:  supportGraceData,
	},
	"support_fee_grace_reminder": {
		TemplateID:   "subscription/grace_reminder",
		EmailSubject: "Action required: pay your support invoice to keep editing your data",
		DataBuilder:  supportGraceData,
	},
}

// upgradeTrialData fills the upgrade trial and plan change templates. Every key the templates
// use is copied through; action_link prefers the subscriptions billing page sent by
// subscriptions-api.
func upgradeTrialData(payload map[string]any, tenantWebsite string) map[string]any {
	out := map[string]any{
		"name":        "Admin",
		"action_link": firstNonEmpty(payload["manage_url"], fmt.Sprintf("%s/settings/subscription", tenantWebsite)),
	}
	for _, k := range []string{"plan_name", "base_plan_name", "upgrade_plan_name", "from_plan_name", "days",
		"days_remaining", "ends_at", "upgrade_at", "effective_at", "outcome"} {
		if v, ok := payload[k]; ok {
			out[k] = v
		}
	}
	return out
}

// supportGraceData fills the grace-reminder template for a support charge. plan_name carries the
// agreement name ("Hosting and support", or the special agreement's name).
func supportGraceData(payload map[string]any, tenantWebsite string) map[string]any {
	amount := payload["amount"]
	if cur, _ := payload["currency"].(string); cur != "" && amount != nil {
		amount = fmt.Sprintf("%s %v", cur, amount)
	}
	return map[string]any{
		"name":           "Admin",
		"plan_name":      payload["agreement_name"],
		"days_remaining": payload["days_remaining"],
		"grace_ends_at":  payload["grace_ends_at"],
		"amount":         amount,
		"invoice_number": payload["invoice_number"],
		"payment_link":   firstNonEmpty(payload["pay_link"], fmt.Sprintf("%s/settings/subscription", tenantWebsite)),
	}
}

// subscriptionIdempotencyKey is stable per event. Reminder events repeat daily for the same
// aggregate, so they key on the event id; one-shot events keep the per-aggregate key.
func subscriptionIdempotencyKey(evt subscriptionEvent) string {
	switch evt.EventType {
	case "grace_reminder", "support_fee_grace_reminder",
		// A tenant can run several trials and schedules over time on one subscription.
		"upgrade_trial_started", "upgrade_trial_ending", "upgrade_trial_ended", "plan_change_scheduled":
		if evt.ID != "" {
			return fmt.Sprintf("subscription-%s-%s", evt.EventType, evt.ID)
		}
	}
	return fmt.Sprintf("subscription-%s-%s", evt.EventType, evt.AggregateID)
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
// subscriptions-service and dispatches notification emails to tenant admins. SMS and WhatsApp
// credits are no longer fulfilled here: subscriptions-api owns them end to end.
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

		if evt.EventType == eventAutoTopupFailed {
			tid := evt.TenantID
			if tid == "" {
				tid, _ = evt.Payload["tenant_id"].(string)
			}
			if tid == "" {
				logg.Warn("auto top-up notice: no tenant_id, skipping")
				_ = m.Ack()
				return
			}
			ti, err := tr.resolve(ctx, tid)
			if err != nil {
				logg.Error("auto top-up notice: failed to resolve tenant", zap.String("tenant_id", tid), zap.Error(err))
				_ = m.Nak()
				return
			}
			if err := sendAutoTopupFailed(ctx, nc, cfg, evt, tid, ti, logg); err != nil {
				logg.Error("auto top-up notice failed", zap.Error(err))
				_ = m.Nak()
				return
			}
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
			logg.Warn("subscription event: tenant has no contact_email, skipping email", zap.String("tenant_id", tenantID))
			sendBillingWhatsApp(ctx, nc, cfg, evt, tenantID, ti, logg)
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
			IdempotencyKey: subscriptionIdempotencyKey(evt),
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
		// Bills and payment reminders also go to the tenant's billing phone over WhatsApp.
		sendBillingWhatsApp(ctx, nc, cfg, evt, tenantID, ti, logg)
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
