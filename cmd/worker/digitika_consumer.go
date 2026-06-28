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

// digitikaEvent matches the platform-standard shared-events envelope published by
// codevertex-website's events.ts using the shared-events format:
//
//	subject: digitika.{event_type}  (e.g. digitika.enrollment.confirmed)
//	body:    { id, event_type, aggregate_type, aggregate_id, tenant_id, tenant_slug, payload, ... }
type digitikaEvent struct {
	ID            string         `json:"id"`
	EventType     string         `json:"event_type"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	TenantID      string         `json:"tenant_id"`   // slug: "codevertex"
	TenantSlug    string         `json:"tenant_slug"` // slug: "codevertex"
	Payload       map[string]any `json:"payload"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

var digitikaOrdinals = []string{"", "1st", "2nd", "3rd", "4th", "5th", "6th", "7th", "8th", "9th", "10th"}

func ordinalLabel(n int) string {
	if n >= 1 && n < len(digitikaOrdinals) {
		return digitikaOrdinals[n]
	}
	return fmt.Sprintf("%dth", n)
}

// startDigitikaConsumer subscribes to digitika.> JetStream events published by
// codevertex-website and dispatches enrollment confirmation and installment receipt emails.
func startDigitikaConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping digitika consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt digitikaEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("digitika event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		payload := evt.Payload
		if payload == nil {
			logg.Warn("digitika event: empty payload", zap.String("event_type", evt.EventType))
			_ = m.Ack()
			return
		}

		// Resolve tenant by slug ("codevertex") to get its UUID for messaging.Message.
		// TenantID from the event is a slug, not a UUID — resolveBySlug handles this.
		tenantID := ""
		slug := evt.TenantSlug
		if slug == "" {
			slug = evt.TenantID
		}
		if ti, err := tr.resolveBySlug(ctx, slug); err == nil {
			tenantID = ti.ID.String()
		} else {
			logg.Warn("digitika event: tenant slug not resolved, using slug as fallback",
				zap.String("slug", slug), zap.Error(err))
			tenantID = slug
		}

		recipientEmail, _ := payload["studentEmail"].(string)
		if recipientEmail == "" {
			logg.Warn("digitika event: no studentEmail in payload", zap.String("event_type", evt.EventType))
			_ = m.Ack()
			return
		}

		var msg messaging.Message

		switch evt.EventType {
		case "enrollment.confirmed":
			studentName, _ := payload["studentName"].(string)
			courseName, _ := payload["courseName"].(string)
			paymentPlan, _ := payload["paymentPlan"].(string)
			totalAmount, _ := payload["totalAmount"].(float64)
			currency, _ := payload["currency"].(string)
			studentId, _ := payload["studentId"].(string)
			portalLink, _ := payload["portalLink"].(string)
			installmentsSummary, _ := payload["installmentsSummary"].(string)
			enrollmentId, _ := payload["enrollmentId"].(string)

			msg = messaging.Message{
				TenantID:    tenantID,
				Channel:     "email",
				TemplateID:  "digitika/enrollment_confirmed",
				SenderScope: messaging.SenderScopeTenant,
				Target:      messaging.TargetCustomer,
				To:          []string{recipientEmail},
				Data: map[string]any{
					"student_name":         studentName,
					"course_name":          courseName,
					"payment_plan":         paymentPlan,
					"total_amount":         fmt.Sprintf("%.0f", totalAmount),
					"currency":             currency,
					"student_id":           studentId,
					"cohort_name":          "",
					"portal_link":          portalLink,
					"installments_summary": installmentsSummary,
				},
				Metadata: map[string]any{
					"subject": fmt.Sprintf("Enrollment Confirmed — %s", courseName),
				},
				RequestID:      uuid.New().String(),
				IdempotencyKey: fmt.Sprintf("digitika-enrollment-%s", enrollmentId),
				QueuedAt:       time.Now(),
			}

		case "payment.succeeded":
			studentName, _ := payload["studentName"].(string)
			courseName, _ := payload["courseName"].(string)
			installmentNoF, _ := payload["installmentNo"].(float64)
			installmentNo := int(installmentNoF)
			amountPaid, _ := payload["amountPaid"].(float64)
			currency, _ := payload["currency"].(string)
			paymentRef, _ := payload["paymentRef"].(string)
			remainingBalance, _ := payload["remainingBalance"].(float64)
			nextDate, _ := payload["nextInstallmentDate"].(string)
			nextAmtF, _ := payload["nextInstallmentAmount"].(float64)
			studentId, _ := payload["studentId"].(string)
			portalLink, _ := payload["portalLink"].(string)
			enrollmentId, _ := payload["enrollmentId"].(string)

			nextAmt := ""
			if nextAmtF > 0 {
				nextAmt = fmt.Sprintf("%.0f", nextAmtF)
			}
			remainingStr := ""
			if remainingBalance > 0 {
				remainingStr = fmt.Sprintf("%.0f", remainingBalance)
			}

			msg = messaging.Message{
				TenantID:    tenantID,
				Channel:     "email",
				TemplateID:  "digitika/installment_receipt",
				SenderScope: messaging.SenderScopeTenant,
				Target:      messaging.TargetCustomer,
				To:          []string{recipientEmail},
				Data: map[string]any{
					"student_name":            studentName,
					"course_name":             courseName,
					"payment_label":           ordinalLabel(installmentNo),
					"amount_paid":             fmt.Sprintf("%.0f", amountPaid),
					"currency":                currency,
					"payment_ref":             paymentRef,
					"remaining_balance":       remainingStr,
					"next_installment_date":   nextDate,
					"next_installment_amount": nextAmt,
					"student_id":              studentId,
					"portal_link":             portalLink,
				},
				Metadata: map[string]any{
					"subject": fmt.Sprintf("Payment Received — %s Installment: %s", ordinalLabel(installmentNo), courseName),
				},
				RequestID:      uuid.New().String(),
				IdempotencyKey: fmt.Sprintf("digitika-payment-%s-%d", enrollmentId, installmentNo),
				QueuedAt:       time.Now(),
			}

		default:
			logg.Debug("digitika event: unhandled event_type, skipping", zap.String("event_type", evt.EventType))
			_ = m.Ack()
			return
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("digitika event: failed to dispatch notification",
				zap.String("event_type", evt.EventType),
				zap.String("to", recipientEmail),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("digitika notification dispatched",
			zap.String("event_type", evt.EventType),
			zap.String("template", msg.TemplateID),
			zap.String("to", recipientEmail),
		)
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "digitika", "digitika.>", "notifications-digitika", handler,
		nats.BindStream("digitika"),
		nats.Durable("notifications-digitika"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
