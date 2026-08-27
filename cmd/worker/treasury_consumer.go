package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/billing"
	"github.com/bengobox/notifications-api/internal/providers"
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
	// NOTE: payment.created (intent creation, "invoice only") intentionally sends NO email.
	// A "payment receipt" at intent creation is misleading — the payment_method and status
	// are still "pending" because no charge has occurred yet. The customer receives the real
	// confirmation via finance/payment_success on the actual payment.succeeded event below.
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
	// Explicit "Send Payment Received Notification" (invoice View Payments modal action) —
	// confirms ONE recorded payment to the customer. Reuses the payment-receipt template.
	// NOTE: the automatic treasury.payment_received event (now fired for partials too) is
	// deliberately NOT mapped — customer comms stay an explicit action, not per-payment spam.
	"payment_received_notification": {
		TemplateID:   "finance/payment_receipt",
		EmailSubject: "Payment received — thank you",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			name, _ := payload["customer_name"].(string)
			if name == "" {
				name = "Customer"
			}
			return map[string]any{
				"name":           name,
				"amount":         fmt.Sprintf("%v %v", payload["currency"], payload["amount"]),
				"transaction_id": payload["reference"],
				"payment_method": payload["method"],
				"payment_date":   formatEventDate(payload["paid_at"]),
				"invoice_number": payload["invoice_number"],
				"receipt_link":   fmt.Sprintf("%s/invoices/%s", serviceURL("NOTIFICATIONS_TREASURY_APP_URL", tenantWebsite), payload["invoice_id"]),
			}
		},
	},
	// OTP second factor for money-movement approvals (REQ-004): emails the 6-digit code to
	// the APPROVER (payload approver_email — normalized onto customer_email pre-dispatch).
	// Reuses the auth OTP template (same 5-minute-expiry wording).
	"approval_otp_requested": {
		TemplateID:   "auth/otp_verification",
		EmailSubject: "Your approval verification code",
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"name": "Approver",
				"otp":  payload["otp_code"],
			}
		},
	},
	// Invoice issuance: treasury's SendInvoice publishes this on every "Send" action, but until
	// now nothing in this table handled it — the event was silently dropped ("unhandled type,
	// skipping"), so a document showing status "sent" was never actually emailed to the
	// customer even though the finance/invoice_sent template already existed unused. pdf_url is
	// the public, unauthenticated branded PDF (SendInvoice's own comment: "serves the branded
	// invoice without auth"); pay_url is the public Paystack checkout link — both only present
	// when treasury's PUBLIC_UI_BASE/PUBLIC_API_BASE are configured, hence the template's own
	// `{{ if .payment_link }}` guard and invoice_link falling back to a treasury-ui deep link.
	"invoice_sent": {
		TemplateID:   "finance/invoice_sent",
		EmailSubject: "Invoice sent",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			name, _ := payload["customer_name"].(string)
			if name == "" {
				name = "Customer"
			}
			invoiceLink, _ := payload["pdf_url"].(string)
			if invoiceLink == "" {
				invoiceLink = fmt.Sprintf("%s/invoices/%s", serviceURL("NOTIFICATIONS_TREASURY_APP_URL", tenantWebsite), payload["invoice_id"])
			}
			return map[string]any{
				"name":           name,
				"invoice_number": payload["invoice_number"],
				"amount":         fmt.Sprintf("%v %v", payload["currency"], payload["amount"]),
				"due_date":       formatEventDate(payload["due_date"]),
				"invoice_link":   invoiceLink,
				"payment_link":   payload["pay_url"],
			}
		},
	},
	// AR dunning: treasury's dunning worker emits one reminder per invoice per overdue tier.
	// Reuses the existing invoice_overdue template (same fields). Recipient is the invoice's
	// customer_email (carried in the event payload).
	"dunning.reminder_sent": {
		TemplateID:   "finance/invoice_overdue",
		EmailSubject: "Payment reminder: invoice overdue",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			name, _ := payload["customer_name"].(string)
			if name == "" {
				name = "Customer"
			}
			return map[string]any{
				"name":           name,
				"invoice_number": payload["invoice_number"],
				"amount":         payload["amount"],
				"due_date":       payload["due_date"],
				"days_overdue":   payload["days_overdue"],
				"payment_link":   fmt.Sprintf("%s/invoices/%s", serviceURL("NOTIFICATIONS_TREASURY_APP_URL", tenantWebsite), payload["invoice_id"]),
			}
		},
	},
}

// formatEventDate renders an RFC3339 timestamp (as emitted by Go's time.Time JSON
// marshaling) into a friendly date for emails. Falls back to the raw value. Shared
// across consumers in this package (e.g. subscription invoice_generated).
func formatEventDate(v any) string {
	s, _ := v.(string)
	if s == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Format("02 Jan 2006")
	}
	return s
}

// startTreasuryConsumer subscribes to treasury.> events from the treasury-api
// JetStream stream and dispatches payment notification emails. It also handles
// credit top-up (reference_type=topup) and WhatsApp subscription activation
// (reference_type=whatsapp_subscription).
func startTreasuryConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, billingSvc *billing.Service, whatsappSubsSvc *billing.WhatsAppSubscriptionService, pm *providers.Manager, dbPool *pgxpool.Pool, logg *zap.Logger) {
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
				// A tenant is never blocked from buying more SMS credit than the platform's own
				// real Africa's Talking wallet currently covers — but if this purchase pushes
				// total outstanding tenant demand past what that real wallet can fund, alert the
				// platform owner to recharge it. Best-effort: never fails/retries the topup itself
				// over an alert-check hiccup.
				if creditType == "SMS" {
					checkSMSWalletCapacityAndAlert(ctx, nc, cfg, billingSvc, pm, dbPool, logg)
				}
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

		// Approval OTPs go to the APPROVER, never a customer or the tenant fallback contact —
		// delivering a second factor to the wrong inbox defeats it. Drop when absent.
		if eventType == "approval_otp_requested" {
			if ae, _ := payload["approver_email"].(string); ae != "" {
				payload["customer_email"] = ae
			} else {
				logg.Warn("approval otp: no approver_email in payload — dropping", zap.String("tenant_id", tenantID))
				_ = m.Ack()
				return
			}
		}

		// Automatic per-payment result emails are OPT-IN: treasury embeds
		// notification.enabled from the tenant's config. Absent or false means the tenant
		// never opted in — skip. Without this gate every POS sale generated a
		// "Payment successful" email, flooding the shared SMTP account (and, before the
		// fallback fix below, all of them landed in the tenant admin's inbox).
		if eventType == "payment.succeeded" || eventType == "payment.failed" {
			notif, _ := payload["notification"].(map[string]any)
			if enabled, _ := notif["enabled"].(bool); !enabled {
				logg.Debug("treasury event: payment emails not enabled for tenant, skipping",
					zap.String("tenant_id", tenantID), zap.String("type", eventType))
				_ = m.Ack()
				return
			}
		}

		// Get customer email from the event payload (immediate-settle events carry it as
		// notification.recipient_email instead of top-level customer_email). Customer-facing
		// events must NEVER fall back to the tenant contact: a walk-in POS payment has no
		// customer email, and mailing the tenant admin instead is pure spam. Only
		// admin-facing events (payouts) may fall back to the tenant contact.
		customerEmail, _ := payload["customer_email"].(string)
		if customerEmail == "" {
			if notif, _ := payload["notification"].(map[string]any); notif != nil {
				customerEmail, _ = notif["recipient_email"].(string)
			}
		}
		if customerEmail == "" {
			if eventType != "payout.completed" {
				logg.Debug("treasury event: no customer_email on customer-facing event, skipping",
					zap.String("tenant_id", tenantID), zap.String("type", eventType))
				_ = m.Ack()
				return
			}
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
		if eventType == "payout.completed" || eventType == "approval_otp_requested" {
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

		// Dunning fires up to 3 tiers for the same invoice — key each tier independently so a later
		// tier isn't mistaken for a duplicate of the first.
		if eventType == "dunning.reminder_sent" {
			msg.IdempotencyKey = fmt.Sprintf("treasury-dunning-%s-%v", aggregateID, payload["reminder_number"])
			// Escalate the wording by tier (gentle/firm/urgent) using the schedule's template name;
			// any other/empty value keeps the default invoice_overdue template.
			if t, _ := payload["template"].(string); t == "gentle" || t == "firm" || t == "urgent" {
				msg.TemplateID = "finance/dunning_" + t
			}
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

	eventslib.SubscribeQueueWithRebind(logg, js, "treasury", "treasury.>", "notifications-treasury-payments", handler,
		nats.BindStream("treasury"),
		nats.Durable("notifications-treasury-payments"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}

// checkSMSWalletCapacityAndAlert compares total outstanding tenant SMS-credit demand (every
// tenant's purchased-but-unspent balance, converted to a real SMS count at the tenant-facing
// rate) against how many real SMS the platform's own Africa's Talking account balance can
// currently fund (at the real provider cost). A tenant purchase is NEVER blocked by this — it
// only decides whether to alert the platform owner that the real wallet needs recharging to
// cover what's now been sold. Best-effort throughout: every failure just skips the alert
// silently rather than risking any impact on the topup that already succeeded.
func checkSMSWalletCapacityAndAlert(ctx context.Context, nc *nats.Conn, cfg *config.Config, billingSvc *billing.Service, pm *providers.Manager, dbPool *pgxpool.Pool, logg *zap.Logger) {
	smsProv, err := pm.GetSMSProvider(ctx, pm.PlatformID, "")
	if err != nil {
		logg.Debug("sms wallet capacity check: no sms provider resolved, skipping", zap.Error(err))
		return
	}
	balProv, ok := smsProv.(interface{ GetBalance(context.Context) (float64, error) })
	if !ok {
		return // active provider has no real-balance concept (e.g. Twilio) — nothing to compare
	}
	realBalance, err := balProv.GetBalance(ctx)
	if err != nil {
		logg.Warn("sms wallet capacity check: real balance lookup failed, skipping", zap.Error(err))
		return
	}

	providerCost := billingSvc.PlatformProviderCost(ctx, "SMS")
	tenantRate := billingSvc.PlatformCostPerSms(ctx)
	if providerCost <= 0 || tenantRate <= 0 {
		return
	}

	outstandingKES, err := billingSvc.TotalOutstandingBalance(ctx, "SMS")
	if err != nil {
		logg.Warn("sms wallet capacity check: outstanding balance query failed, skipping", zap.Error(err))
		return
	}

	outstandingSMSCount := outstandingKES / tenantRate
	supportableSMSCount := realBalance / providerCost
	if outstandingSMSCount <= supportableSMSCount {
		return // real wallet still covers everything already sold — nothing to alert
	}

	shortfallSMS := outstandingSMSCount - supportableSMSCount
	shortfallKES := shortfallSMS * providerCost

	ccEmail := platformCCEmail(ctx, dbPool)
	if ccEmail == "" {
		logg.Warn("sms wallet capacity shortfall detected but no platform alert email configured",
			zap.Float64("outstanding_sms", outstandingSMSCount), zap.Float64("supportable_sms", supportableSMSCount))
		return
	}

	msg := messaging.Message{
		TenantID:    pm.PlatformID,
		Channel:     "email",
		TemplateID:  "shared/generic_notification",
		SenderScope: messaging.SenderScopePlatform,
		Target:      messaging.TargetPlatformAdmin,
		To:          []string{ccEmail},
		Data: map[string]any{
			"name":  "Platform Owner",
			"title": "Africa's Talking wallet needs recharging",
			"message": fmt.Sprintf(
				"Tenants have now purchased more SMS credit than the platform's real Africa's Talking account balance can currently fund.",
			),
			"details": fmt.Sprintf(
				"Real AT balance: KES %.2f (supports ~%.0f SMS at KES %.2f/SMS)<br>"+
					"Outstanding tenant-purchased credit: KES %.2f (~%.0f SMS owed at KES %.2f/SMS)<br>"+
					"Shortfall: ~%.0f SMS (recharge the AT wallet by at least KES %.2f to cover it)",
				realBalance, supportableSMSCount, providerCost,
				outstandingKES, outstandingSMSCount, tenantRate,
				shortfallSMS, shortfallKES,
			),
		},
		Metadata:       map[string]any{"subject": "Action needed: recharge Africa's Talking SMS wallet"},
		RequestID:      uuid.New().String(),
		IdempotencyKey: fmt.Sprintf("sms-wallet-shortfall-%s", time.Now().Format("2006-01-02T15")), // at most once/hour
		QueuedAt:       time.Now(),
	}
	if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
		logg.Warn("sms wallet capacity alert: failed to publish", zap.Error(err))
		return
	}
	logg.Info("sms wallet capacity shortfall alert sent",
		zap.Float64("outstanding_sms", outstandingSMSCount),
		zap.Float64("supportable_sms", supportableSMSCount),
		zap.Float64("shortfall_kes", shortfallKES),
	)
}
