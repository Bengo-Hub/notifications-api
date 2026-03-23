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

// posEvent is the shared-events envelope from pos-api.
type posEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

type posNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(payload map[string]any, tenantWebsite string) map[string]any
}

var posMappings = map[string]posNotificationMapping{
	"order.status_changed": {
		TemplateID:   "pos/pos_order_ready",
		EmailSubject: "Your POS order is ready",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":       "Customer",
				"order_id":   payload["order_number"],
				"outlet_name": "",
				"order_link": fmt.Sprintf("%s/orders", tenantWebsite),
			}
		},
	},
	"payment.recorded": {
		TemplateID:   "pos/pos_payment_receipt",
		EmailSubject: "Payment receipt",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":           "Customer",
				"receipt_number": payload["payment_id"],
				"order_id":       payload["order_number"],
				"total_amount":   payload["amount"],
				"payment_method": payload["payment_method"],
				"receipt_link":   fmt.Sprintf("%s/receipts", tenantWebsite),
			}
		},
	},
}

// startPosConsumer subscribes to pos.> events and dispatches POS notification emails.
func startPosConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping pos consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt posEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("pos event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		// Only send order_ready when status changed to completed
		if evt.EventType == "order.status_changed" {
			newStatus, _ := evt.Payload["new_status"].(string)
			if newStatus != "completed" {
				_ = m.Ack()
				return
			}
		}

		mapping, ok := posMappings[evt.EventType]
		if !ok {
			logg.Debug("pos event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		tenantID := evt.TenantID
		if tenantID == "" {
			logg.Warn("pos event: no tenant_id, skipping")
			_ = m.Ack()
			return
		}

		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			logg.Error("pos event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		if ti.ContactEmail == "" {
			logg.Warn("pos event: tenant has no contact_email, skipping")
			_ = m.Ack()
			return
		}

		msg := messaging.Message{
			TenantID:   tenantID,
			Channel:    "email",
			TemplateID: mapping.TemplateID,
			To:         []string{ti.ContactEmail},
			Data:       mapping.DataBuilder(evt.Payload, ti.Website),
			Metadata: map[string]any{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("pos-%s-%s", evt.EventType, evt.AggregateID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("pos event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}

		logg.Info("pos notification dispatched", zap.String("type", evt.EventType), zap.String("template", mapping.TemplateID))
		_ = m.Ack()
	}

	_, err := js.Subscribe("pos.>", handler,
		nats.BindStream("pos"),
		nats.Durable("notifications-pos-orders"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	if err != nil {
		logg.Warn("pos consumer subscription failed (pos stream may not exist yet)", zap.Error(err))
		return
	}

	logg.Info("pos consumer started", zap.String("subject", "pos.>"))
}
