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

// orderEvent is the CloudEvents envelope from ordering-service.
type orderEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenantId"`
	Data     map[string]interface{} `json:"data"`
}

// orderNotificationMapping maps event types to notification details.
type orderNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(data map[string]interface{}, tenantWebsite string) map[string]interface{}
}

var orderMappings = map[string]orderNotificationMapping{
	"ordering.order.confirmed": {
		TemplateID:   "email/ordering/order_placed",
		EmailSubject: "Your order has been confirmed",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":                data["customer_name"],
				"order_id":            data["order_id"],
				"total_amount":        data["total_amount"],
				"estimated_prep_time": data["estimated_prep_time"],
				"delivery_address":    data["delivery_address"],
				"order_link":          fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
	"ordering.order.ready": {
		TemplateID:   "email/ordering/order_ready",
		EmailSubject: "Your order is ready",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       data["customer_name"],
				"order_id":   data["order_id"],
				"order_link": fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
	"ordering.order.out_for_delivery": {
		TemplateID:   "email/ordering/order_out_for_delivery",
		EmailSubject: "Your order is out for delivery",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       data["customer_name"],
				"order_id":   data["order_id"],
				"rider_name": data["rider_name"],
				"order_link": fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
	"ordering.order.completed": {
		TemplateID:   "email/ordering/order_delivered",
		EmailSubject: "Your order has been delivered",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       data["customer_name"],
				"order_id":   data["order_id"],
				"order_link": fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
	"ordering.order.cancelled": {
		TemplateID:   "email/ordering/order_cancelled",
		EmailSubject: "Your order has been cancelled",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":          data["customer_name"],
				"order_id":      data["order_id"],
				"cancel_reason": data["cancel_reason"],
				"order_link":    fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
	"ordering.order.refunded": {
		TemplateID:   "email/ordering/order_refunded",
		EmailSubject: "Your refund has been processed",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_number": data["order_number"],
				"amount":       data["total_amount"],
				"currency":     data["currency"],
				"reason":       data["reason"],
				"order_link":   fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
	"ordering.order.scheduled": {
		TemplateID:   "email/ordering/order_scheduled",
		EmailSubject: "Your order has been scheduled",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":          data["customer_name"],
				"order_number":  data["order_number"],
				"scheduled_for": data["scheduled_for"],
				"total_amount":  data["total_amount"],
				"currency":      data["currency"],
				"order_link":    fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
	"ordering.order.for_pickup": {
		TemplateID:   "email/ordering/order_for_pickup",
		EmailSubject: "Your order is ready for pickup",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_number": data["order_number"],
				"outlet_name":  data["outlet_name"],
				"pickup_time":  data["pickup_time"],
				"order_link":   fmt.Sprintf("%s/orders/%s", tenantWebsite, data["order_id"]),
			}
		},
	},
}

// startOrderConsumer subscribes to ordering.order.> events and dispatches
// customer notifications for order status changes.
func startOrderConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping order consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt orderEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("order event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		mapping, ok := orderMappings[evt.Type]
		if !ok {
			logg.Debug("order event: unhandled type, skipping", zap.String("type", evt.Type))
			_ = m.Ack()
			return
		}

		// Extract customer email from event data
		email, _ := evt.Data["customer_email"].(string)
		if email == "" {
			logg.Warn("order event: no customer_email in data, skipping", zap.String("type", evt.Type))
			_ = m.Ack()
			return
		}

		// Resolve tenant website for building order links
		tenantWebsite := ""
		if ti, err := tr.resolve(ctx, evt.TenantID); err == nil {
			tenantWebsite = ti.Website
		} else {
			logg.Warn("order event: could not resolve tenant, using empty website", zap.String("tenant_id", evt.TenantID), zap.Error(err))
		}

		orderID, _ := evt.Data["order_id"].(string)

		msg := messaging.Message{
			TenantID:    evt.TenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetCustomer,
			To:          []string{email},
			Data:        mapping.DataBuilder(evt.Data, tenantWebsite),
			Metadata: map[string]interface{}{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("order-%s-%s", evt.Type, orderID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("order event: failed to dispatch notification",
				zap.String("type", evt.Type),
				zap.String("order_id", orderID),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("order notification dispatched",
			zap.String("type", evt.Type),
			zap.String("template", mapping.TemplateID),
			zap.String("order_id", orderID),
			zap.String("to", email),
		)
		_ = m.Ack()
	}

	_, err := js.Subscribe("ordering.order.>", handler,
		nats.BindStream("ordering"),
		nats.Durable("notifications-ordering-status"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	if err != nil {
		logg.Warn("order consumer subscription failed (ordering stream may not exist yet)", zap.Error(err))
		return
	}

	logg.Info("order consumer started", zap.String("subject", "ordering.order.>"))
}
