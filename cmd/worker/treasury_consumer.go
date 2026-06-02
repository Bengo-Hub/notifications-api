package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/billing"
)

// treasuryEvent is the outbox envelope from treasury-api.
// The shared-events publisher marshals OutboxRecord.Payload (which is the JSON
// of the event data map) and publishes to subject "treasury.{eventType}".
type treasuryEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

// treasuryNotificationMapping maps event types to notification details.
type treasuryNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(payload map[string]any, tenantWebsite string) map[string]any
}

var treasuryMappings = map[string]treasuryNotificationMapping{
	"payment.succeeded": {
		TemplateID:   "finance/payment_success",
		EmailSubject: "Payment successful",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":       "Customer",
				"amount":     fmt.Sprintf("%s %s", payload["amount"], payload["currency"]),
				"order_id":   payload["reference_id"],
				"order_link": fmt.Sprintf("%s/orders/%s", serviceURL("NOTIFICATIONS_ORDERING_APP_URL", tenantWebsite), payload["reference_id"]),
			}
		},
	},
	"payment.failed": {
		TemplateID:   "finance/payment_failed",
		EmailSubject: "Payment failed",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":       "Customer",
				"order_id":   payload["reference_id"],
				"retry_link": fmt.Sprintf("%s/orders/%s/pay", serviceURL("NOTIFICATIONS_ORDERING_APP_URL", tenantWebsite), payload["reference_id"]),
			}
		},
	},
	"payment.created": {
		TemplateID:   "finance/payment_receipt",
		EmailSubject: "Payment receipt",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Customer",
				"amount":         fmt.Sprintf("%s %s", payload["amount"], payload["currency"]),
				"transaction_id": payload["intent_id"],
				"payment_method": payload["payment_method"],
				"receipt_link":   fmt.Sprintf("%s/payments", serviceURL("NOTIFICATIONS_TREASURY_APP_URL", tenantWebsite)),
			}
		},
	},
	"payout.completed": {
		TemplateID:   "finance/payout_completed",
		EmailSubject: "Your payout has been processed",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":             "Admin",
				"amount":           fmt.Sprintf("%s %s", payload["net_amount"], payload["currency"]),
				"payout_id":        payload["reference"],
				"payout_method":    "Bank Transfer",
				"reference_number": payload["transfer_code"],
				"payout_link":      fmt.Sprintf("%s/dashboard/settlements", serviceURL("NOTIFICATIONS_TREASURY_APP_URL", tenantWebsite)),
			}
		},
	},
	"refund.completed": {
		TemplateID:   "finance/refund_completed",
		EmailSubject: "Your refund has been processed",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           payload["customer_name"],
				"amount":         payload["amount"],
				"currency":       payload["currency"],
				"reference_type": payload["reference_type"],
				"reference_id":   payload["reference_id"],
				"reason":         payload["reason"],
				"action_link":    fmt.Sprintf("%s/payments", serviceURL("NOTIFICATIONS_TREASURY_APP_URL", tenantWebsite)),
			}
		},
	},
}

// startTreasuryConsumer subscribes to treasury.> events from the treasury-api
// JetStream stream and dispatches payment notification emails. It also handles
// credit top-up (reference_type=topup) and WhatsApp subscription activation
// (reference_type=whatsapp_subscription).
func startTreasuryConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, billingSvc *billing.Service, whatsappSubsSvc *billing.WhatsAppSubscriptionService, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping treasury consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		// Treasury outbox publisher sends raw payload bytes — try parsing as
		// the event data map directly first, fall back to envelope.
		var payload map[string]any
		var eventType string
		var tenantID string
		var aggregateID string

		// Try envelope first
		var evt treasuryEvent
		if err := json.Unmarshal(m.Data, &evt); err == nil && evt.EventType != "" {
			eventType = evt.EventType
			tenantID = evt.TenantID
			aggregateID = evt.AggregateID
			payload = evt.Payload
			if payload == nil {
				payload = make(map[string]any)
			}
		} else {
			// Try raw payload map
			if err := json.Unmarshal(m.Data, &payload); err != nil {
				logg.Error("treasury event: unmarshal failed", zap.Error(err))
				_ = m.Ack()
				return
			}
			eventType, _ = payload["event_type"].(string)
			tenantID, _ = payload["tenant_id"].(string)
			aggregateID, _ = payload["aggregate_id"].(string)
		}

		// Digitika enrollment payments are handled by codevertex-website's treasury
		// webhook which sends enriched emails (student name, course, portal link).
		// Skip here to avoid duplicate generic payment emails.
		referenceType, _ := payload["reference_type"].(string)
		if referenceType == "digitika_enrollment" {
			logg.Debug("treasury event: digitika_enrollment handled by codevertex-website webhook, skipping", zap.String("type", eventType))
			_ = m.Ack()
			return
		}

		// Handle credit top-up completion: when a tenant pays for SMS/WhatsApp credits,
		// treasury publishes payment.succeeded with reference_type=topup.
		// Credit the tenant balance instead of sending a generic payment email.
		if eventType == "payment.succeeded" && referenceType == "topup" && billingSvc != nil {
			if tenantID == "" {
				logg.Warn("treasury topup: no tenant_id, skipping", zap.String("type", eventType))
				_ = m.Ack()
				return
			}
			tid, tidErr := uuid.Parse(tenantID)
			if tidErr != nil {
				logg.Warn("treasury topup: invalid tenant_id", zap.String("tenant_id", tenantID), zap.Error(tidErr))
				_ = m.Ack()
				return
			}

			// Extract credit_type and amount from metadata
			meta, _ := payload["metadata"].(map[string]any)
			creditType := "SMS"
			if meta != nil {
				if ct, ok := meta["credit_type"].(string); ok && ct != "" {
					creditType = ct
				}
			}

			var amount float64
			switch v := payload["amount"].(type) {
			case float64:
				amount = v
			case string:
				_, _ = fmt.Sscanf(v, "%f", &amount)
			}

			referenceID, _ := payload["intent_id"].(string)
			if referenceID == "" {
				referenceID = aggregateID
			}

			if amount > 0 {
				if topupErr := billingSvc.TopUpCredits(ctx, tid, creditType, amount, referenceID); topupErr != nil {
					logg.Error("treasury topup: failed to credit balance",
						zap.String("tenant_id", tenantID),
						zap.String("credit_type", creditType),
						zap.Float64("amount", amount),
						zap.Error(topupErr),
					)
					_ = m.Nak()
					return
				}
				logg.Info("treasury topup: credits added",
					zap.String("tenant_id", tenantID),
					zap.String("credit_type", creditType),
					zap.Float64("amount", amount),
					zap.String("reference_id", referenceID),
				)
			} else {
				logg.Warn("treasury topup: zero or missing amount in payload", zap.String("tenant_id", tenantID))
			}
			_ = m.Ack()
			return
		}

		// Handle WhatsApp subscription activation payment
		if eventType == "payment.succeeded" && referenceType == "whatsapp_subscription" && whatsappSubsSvc != nil {
			if tenantID == "" {
				logg.Warn("treasury whatsapp_subscription: no tenant_id, skipping")
				_ = m.Ack()
				return
			}
			tid, tidErr := uuid.Parse(tenantID)
			if tidErr != nil {
				logg.Warn("treasury whatsapp_subscription: invalid tenant_id", zap.String("tenant_id", tenantID))
				_ = m.Ack()
				return
			}

			meta, _ := payload["metadata"].(map[string]any)
			planIDStr, _ := meta["plan_id"].(string)
			planID, planIDErr := uuid.Parse(planIDStr)
			if planIDErr != nil {
				logg.Error("treasury whatsapp_subscription: invalid plan_id in metadata", zap.String("plan_id", planIDStr))
				_ = m.Ack()
				return
			}

			referenceID, _ := payload["intent_id"].(string)
			if referenceID == "" {
				referenceID = aggregateID
			}

			if activateErr := whatsappSubsSvc.ActivateSubscription(ctx, tid, planID, referenceID); activateErr != nil {
				logg.Error("treasury whatsapp_subscription: failed to activate",
					zap.String("tenant_id", tenantID),
					zap.String("plan_id", planIDStr),
					zap.Error(activateErr),
				)
				_ = m.Nak()
				return
			}
			logg.Info("whatsapp subscription activated via treasury payment",
				zap.String("tenant_id", tenantID),
				zap.String("plan_id", planIDStr),
			)
			_ = m.Ack()
			return
		}

		mapping, ok := treasuryMappings[eventType]
		if !ok {
			logg.Debug("treasury event: unhandled type, skipping", zap.String("type", eventType))
			_ = m.Ack()
			return
		}

		if tenantID == "" {
			logg.Warn("treasury event: no tenant_id, skipping", zap.String("type", eventType))
			_ = m.Ack()
			return
		}

		// Get customer email from event payload, fall back to tenant contact
		customerEmail, _ := payload["customer_email"].(string)
		if customerEmail == "" {
			ti, err := tr.resolve(ctx, tenantID)
			if err != nil {
				// A non-existent / deleted tenant will never resolve, so Nak-looping the message
				// poison-pills the consumer forever. Ack-drop on "not found"; only Nak (retry)
				// genuinely transient failures.
				if strings.Contains(err.Error(), "not found") {
					logg.Warn("treasury event: tenant not found, dropping", zap.String("tenant_id", tenantID), zap.String("type", eventType))
					_ = m.Ack()
					return
				}
				logg.Error("treasury event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
				_ = m.Nak()
				return
			}
			customerEmail = ti.ContactEmail
		}

		if customerEmail == "" {
			logg.Warn("treasury event: no recipient email, skipping", zap.String("type", eventType))
			_ = m.Ack()
			return
		}

		// Resolve tenant website for links
		tenantWebsite := ""
		if ti, err := tr.resolve(ctx, tenantID); err == nil {
			tenantWebsite = ti.Website
		}

		// Determine sender scope and target based on event type:
		// - Payments: tenant sends to customer
		// - Payouts: tenant sends to tenant admin
		senderScope := messaging.SenderScopeTenant
		target := messaging.TargetCustomer
		if eventType == "payout.completed" {
			target = messaging.TargetTenantAdmin
		}

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: senderScope,
			Target:      target,
			To:          []string{customerEmail},
			Data:        mapping.DataBuilder(payload, tenantWebsite),
			Metadata: map[string]any{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("treasury-%s-%s", eventType, aggregateID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("treasury event: failed to dispatch notification",
				zap.String("type", eventType),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("treasury notification dispatched",
			zap.String("type", eventType),
			zap.String("template", mapping.TemplateID),
			zap.String("to", customerEmail),
		)
		_ = m.Ack()
	}

	subscribeWithRetry(ctx, js, logg, "treasury consumer", true, func() (*nats.Subscription, error) {
		return js.Subscribe("treasury.>", handler,
			nats.BindStream("treasury"),
			nats.Durable("notifications-treasury-payments"),
			nats.ManualAck(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(3),
		)
	})
}
