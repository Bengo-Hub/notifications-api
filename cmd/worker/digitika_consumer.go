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

// digitikaEvent is the CloudEvents-style envelope published by codevertex-website's events.ts.
// Subject: digitika.enrollment.confirmed | digitika.payment.succeeded
type digitikaEvent struct {
	ID        string         `json:"id"`
	Type      string         `json:"type"`
	TenantID  string         `json:"tenant_id"` // slug: "codevertex"
	Timestamp string         `json:"timestamp"`
	Data      map[string]any `json:"data"`
}

var digitikaOrdinals = []string{"", "1st", "2nd", "3rd", "4th", "5th", "6th", "7th", "8th", "9th", "10th"}

func ordinalLabel(n int) string {
	if n >= 1 && n < len(digitikaOrdinals) {
		return digitikaOrdinals[n]
	}
	return fmt.Sprintf("%dth", n)
}

// startDigitikaConsumer subscribes to digitika.> events published by codevertex-website
// and dispatches enrollment confirmation and installment receipt emails.
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

		data := evt.Data
		if data == nil {
			logg.Warn("digitika event: empty data payload", zap.String("type", evt.Type))
			_ = m.Ack()
			return
		}

		// Resolve tenant by slug ("codevertex") to get UUID for messaging.Message
		tenantID := ""
		if ti, err := tr.resolveBySlug(ctx, evt.TenantID); err == nil {
			tenantID = ti.ID.String()
		} else {
			logg.Warn("digitika event: tenant slug not resolved, using fallback",
				zap.String("slug", evt.TenantID), zap.Error(err))
			// Fall back to direct tenant ID if it's already a UUID
			tenantID = evt.TenantID
		}

		recipientEmail, _ := data["studentEmail"].(string)
		if recipientEmail == "" {
			logg.Warn("digitika event: no studentEmail in data", zap.String("type", evt.Type))
			_ = m.Ack()
			return
		}

		var msg messaging.Message

		switch evt.Type {
		case "digitika.enrollment.confirmed":
			studentName, _ := data["studentName"].(string)
			courseName, _ := data["courseName"].(string)
			paymentPlan, _ := data["paymentPlan"].(string)
			totalAmount, _ := data["totalAmount"].(float64)
			currency, _ := data["currency"].(string)
			studentId, _ := data["studentId"].(string)
			portalLink, _ := data["portalLink"].(string)
			installmentsSummary, _ := data["installmentsSummary"].(string)
			enrollmentId, _ := data["enrollmentId"].(string)

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

		case "digitika.payment.succeeded":
			studentName, _ := data["studentName"].(string)
			courseName, _ := data["courseName"].(string)
			installmentNoF, _ := data["installmentNo"].(float64)
			installmentNo := int(installmentNoF)
			amountPaid, _ := data["amountPaid"].(float64)
			currency, _ := data["currency"].(string)
			paymentRef, _ := data["paymentRef"].(string)
			remainingBalance, _ := data["remainingBalance"].(float64)
			nextDate, _ := data["nextInstallmentDate"].(string)
			nextAmtF, _ := data["nextInstallmentAmount"].(float64)
			studentId, _ := data["studentId"].(string)
			portalLink, _ := data["portalLink"].(string)
			enrollmentId, _ := data["enrollmentId"].(string)

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
					"student_name":             studentName,
					"course_name":              courseName,
					"payment_label":            ordinalLabel(installmentNo),
					"amount_paid":              fmt.Sprintf("%.0f", amountPaid),
					"currency":                 currency,
					"payment_ref":              paymentRef,
					"remaining_balance":        remainingStr,
					"next_installment_date":    nextDate,
					"next_installment_amount":  nextAmt,
					"student_id":               studentId,
					"portal_link":              portalLink,
				},
				Metadata: map[string]any{
					"subject": fmt.Sprintf("Payment Received — %s Installment: %s", ordinalLabel(installmentNo), courseName),
				},
				RequestID:      uuid.New().String(),
				IdempotencyKey: fmt.Sprintf("digitika-payment-%s-%d", enrollmentId, installmentNo),
				QueuedAt:       time.Now(),
			}

		default:
			logg.Debug("digitika event: unhandled type, skipping", zap.String("type", evt.Type))
			_ = m.Ack()
			return
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("digitika event: failed to dispatch notification",
				zap.String("type", evt.Type),
				zap.String("to", recipientEmail),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("digitika notification dispatched",
			zap.String("type", evt.Type),
			zap.String("template", msg.TemplateID),
			zap.String("to", recipientEmail),
		)
		_ = m.Ack()
	}

	subscribeWithRetry(ctx, js, logg, "digitika consumer", true, func() (*nats.Subscription, error) {
		return js.Subscribe("digitika.>", handler,
			nats.BindStream("digitika"),
			nats.Durable("notifications-digitika"),
			nats.ManualAck(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(3),
		)
	})
}
