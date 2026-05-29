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
	TemplateID    string
	Channel       string // email | sms | push
	EmailSubject  string
	Target        string
	RecipientFunc func(payload map[string]any, fallbackEmail string) (to string, ok bool)
	DataBuilder   func(payload map[string]any, tenantWebsite string) map[string]any
}

// recipientFromPayload returns a string field from payload if non-empty.
func recipientFromPayload(payload map[string]any, key string) (string, bool) {
	v, _ := payload[key].(string)
	return v, v != ""
}

var posMappings = map[string]posNotificationMapping{
	// ---- Customer-facing: order ready + payment receipt ----
	"order.status_changed": {
		TemplateID:   "pos/pos_order_ready",
		Channel:      "email",
		EmailSubject: "Your POS order is ready",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, fallback string) (string, bool) {
			if v, ok := recipientFromPayload(payload, "customer_email"); ok {
				return v, true
			}
			return fallback, fallback != ""
		},
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			name := "Customer"
			if n, ok := payload["customer_name"].(string); ok && n != "" {
				name = n
			}
			return map[string]any{
				"name":        name,
				"order_id":    payload["order_number"],
				"outlet_name": payload["outlet_name"],
				"order_link":  fmt.Sprintf("%s/orders", serviceURL("NOTIFICATIONS_POS_APP_URL", tenantWebsite)),
			}
		},
	},
	"payment.recorded": {
		TemplateID:   "pos/pos_payment_receipt",
		Channel:      "email",
		EmailSubject: "Payment receipt",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, fallback string) (string, bool) {
			if v, ok := recipientFromPayload(payload, "customer_email"); ok {
				return v, true
			}
			return fallback, fallback != ""
		},
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			name := "Customer"
			if n, ok := payload["customer_name"].(string); ok && n != "" {
				name = n
			}
			return map[string]any{
				"name":           name,
				"receipt_number": payload["payment_id"],
				"order_id":       payload["order_number"],
				"total_amount":   payload["amount"],
				"payment_method": payload["payment_method"],
				"receipt_link":   fmt.Sprintf("%s/receipts", serviceURL("NOTIFICATIONS_POS_APP_URL", tenantWebsite)),
			}
		},
	},

	// ---- KDS: waiter called (SMS to waiter phone) ----
	"kds.waiter.called": {
		TemplateID:   "pos/kds_waiter_called",
		Channel:      "sms",
		EmailSubject: "",
		Target:       messaging.TargetStaff,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "waiter_phone")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"order_number": payload["order_number"],
				"table_number": payload["table_number"],
			}
		},
	},

	// ---- Hotel: check-in confirmation (email to guest) ----
	"hotel.check_in": {
		TemplateID:   "pos/hotel_check_in",
		Channel:      "email",
		EmailSubject: "Welcome — your check-in is confirmed",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "guest_email")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"name":         payload["guest_name"],
				"room_number":  payload["room_number"],
				"nights":       payload["nights"],
				"total_charge": payload["total_charge"],
				"check_in":     payload["check_in_date"],
			}
		},
	},

	// ---- Hotel: check-out receipt (email to guest) ----
	"hotel.check_out": {
		TemplateID:   "pos/hotel_check_out",
		Channel:      "email",
		EmailSubject: "Your stay receipt",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "guest_email")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"name":        payload["guest_name"],
				"total_folio": payload["total_folio"],
				"checked_out": payload["checked_out_at"],
			}
		},
	},

	// ---- Appointments: booking confirmation (SMS to client) ----
	"appointment.created": {
		TemplateID:   "pos/appointment_created",
		Channel:      "sms",
		EmailSubject: "",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "client_phone")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"name":          payload["client_name"],
				"service":       payload["service_name"],
				"staff":         payload["staff_name"],
				"date":          payload["appointment_date"],
				"time":          payload["appointment_time"],
				"outlet":        payload["outlet_name"],
			}
		},
	},

	// ---- Appointments: reminder (SMS to client) ----
	"appointment.reminder": {
		TemplateID:   "pos/appointment_reminder",
		Channel:      "sms",
		EmailSubject: "",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "client_phone")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"name":    payload["client_name"],
				"service": payload["service_name"],
				"time":    payload["appointment_time"],
				"outlet":  payload["outlet_name"],
			}
		},
	},

	// ---- Loyalty: points earned (SMS to customer) ----
	"loyalty.points.earned": {
		TemplateID:   "pos/loyalty_points_earned",
		Channel:      "sms",
		EmailSubject: "",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "customer_phone")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			name := "Customer"
			if n, ok := payload["customer_name"].(string); ok && n != "" {
				name = n
			}
			return map[string]any{
				"name":           name,
				"points_earned":  payload["points_earned"],
				"balance":        payload["balance_after"],
				"order_id":       payload["order_id"],
			}
		},
	},

	// ---- Loyalty: tier upgraded (SMS to customer) ----
	"loyalty.tier_upgraded": {
		TemplateID:   "pos/loyalty_tier_upgraded",
		Channel:      "sms",
		EmailSubject: "",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "customer_phone")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			name := "Customer"
			if n, ok := payload["customer_name"].(string); ok && n != "" {
				name = n
			}
			return map[string]any{
				"name":     name,
				"new_tier": payload["new_tier"],
			}
		},
	},

	// ---- Returns: refund processed (SMS to customer) ----
	"return.completed": {
		TemplateID:   "pos/return_completed",
		Channel:      "sms",
		EmailSubject: "",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "customer_phone")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"refund_amount":       payload["refund_amount"],
				"treasury_refund_ref": payload["treasury_refund_ref"],
				"return_type":         payload["return_type"],
			}
		},
	},

	// ---- Layaway: payment due tomorrow (SMS to customer) ----
	"layaway.payment_due": {
		TemplateID:   "pos/layaway_payment_due",
		Channel:      "sms",
		EmailSubject: "",
		Target:       messaging.TargetCustomer,
		RecipientFunc: func(payload map[string]any, _ string) (string, bool) {
			return recipientFromPayload(payload, "customer_phone")
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			name := "Customer"
			if n, ok := payload["customer_name"].(string); ok && n != "" {
				name = n
			}
			return map[string]any{
				"name":        name,
				"balance_due": payload["balance_due"],
				"due_date":    payload["due_date"],
			}
		},
	},

	// ---- Stock: low-stock alert (SMS to outlet manager / admin contact) ----
	"alert.stock_low": {
		TemplateID:   "pos/alert_stock_low",
		Channel:      "sms",
		EmailSubject: "Low stock alert",
		Target:       messaging.TargetStaff,
		RecipientFunc: func(payload map[string]any, fallback string) (string, bool) {
			if phone, ok := recipientFromPayload(payload, "manager_phone"); ok {
				return phone, true
			}
			return fallback, fallback != ""
		},
		DataBuilder: func(payload map[string]any, _ string) map[string]any {
			return map[string]any{
				"item_name":   payload["item_name"],
				"current_qty": payload["current_qty"],
				"outlet_name": payload["outlet_name"],
			}
		},
	},
}

// startPosConsumer subscribes to pos.> events and dispatches POS notifications.
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

		to, ok := mapping.RecipientFunc(evt.Payload, ti.ContactEmail)
		if !ok {
			logg.Warn("pos event: no recipient found, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		metadata := map[string]any{}
		if mapping.EmailSubject != "" {
			metadata["subject"] = mapping.EmailSubject
		}

		msg := messaging.Message{
			TenantID:       tenantID,
			Channel:        mapping.Channel,
			TemplateID:     mapping.TemplateID,
			SenderScope:    messaging.SenderScopeTenant,
			Target:         mapping.Target,
			To:             []string{to},
			Data:           mapping.DataBuilder(evt.Payload, ti.Website),
			Metadata:       metadata,
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

	subscribeWithRetry(ctx, js, logg, "pos consumer", true, func() (*nats.Subscription, error) {
		return js.Subscribe("pos.>", handler,
			nats.BindStream("pos"),
			nats.Durable("notifications-pos-orders"),
			nats.ManualAck(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(3),
		)
	})
}
