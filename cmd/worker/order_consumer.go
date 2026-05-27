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
// Ordering-backend publishes CloudEvents with "type"/"tenantId"/"data" fields
// (not shared-events "event_type"/"aggregate_type"/"tenant_id"/"payload").
type orderEvent struct {
	ID            string                 `json:"id"`
	Type          string                 `json:"type"`
	TenantID      string                 `json:"tenantId"`
	Data          map[string]interface{} `json:"data"`
	// shared-events fallback fields (for forward compatibility)
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	FallbackTID   string                 `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// resolvedType returns the event type, preferring CloudEvents "type" over shared-events "event_type".
func (e *orderEvent) resolvedType() string {
	if e.Type != "" {
		return e.Type
	}
	if e.AggregateType != "" && e.EventType != "" {
		return e.AggregateType + "." + e.EventType
	}
	return e.EventType
}

// resolvedTenantID returns the tenant ID from whichever field is populated.
func (e *orderEvent) resolvedTenantID() string {
	if e.TenantID != "" {
		return e.TenantID
	}
	return e.FallbackTID
}

// resolvedData returns the event data from whichever field is populated.
func (e *orderEvent) resolvedData() map[string]interface{} {
	if e.Data != nil {
		return e.Data
	}
	return e.Payload
}

// orderNotificationMapping maps event types to notification details.
type orderNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(data map[string]interface{}, orderAppURL string) map[string]interface{}
}

// orderAppBaseURL returns the ordering app URL for building "View Order" links.
func orderAppBaseURL(tenantSlug, tenantWebsite string) string {
	return serviceURLWithSlug("NOTIFICATIONS_ORDERING_APP_URL", tenantSlug, tenantWebsite)
}

// orderLink builds the "View Order" URL for customer emails.
// For guest orders (no customer_id), returns a Google Maps link to the outlet.
// For authenticated orders, returns a link to the ordering app order page.
func orderLink(data map[string]interface{}, orderAppURL string) string {
	orderID, _ := data["order_id"].(string)
	return fmt.Sprintf("%s/orders/%s", orderAppURL, orderID)
}

var orderMappings = map[string]orderNotificationMapping{
	"ordering.order.created": {
		TemplateID:   "ordering/order_placed",
		EmailSubject: "Your order has been confirmed",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":                data["customer_name"],
				"order_id":            data["order_id"],
				"total_amount":        data["total_amount"],
				"estimated_prep_time": data["estimated_prep_time"],
				"delivery_address":    data["delivery_address"],
				"order_link":          orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.ready": {
		TemplateID:   "ordering/order_ready",
		EmailSubject: "Your order is ready",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":       data["customer_name"],
				"order_id":   data["order_id"],
				"order_link": orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.out_for_delivery": {
		TemplateID:   "ordering/order_out_for_delivery",
		EmailSubject: "Your order is out for delivery",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":       data["customer_name"],
				"order_id":   data["order_id"],
				"rider_name": data["rider_name"],
				"order_link": orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.completed": {
		TemplateID:   "ordering/order_delivered",
		EmailSubject: "Your order has been delivered",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":       data["customer_name"],
				"order_id":   data["order_id"],
				"order_link": orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.cancelled": {
		TemplateID:   "ordering/order_cancelled",
		EmailSubject: "Your order has been cancelled",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":          data["customer_name"],
				"order_id":      data["order_id"],
				"cancel_reason": data["cancel_reason"],
				"order_link":    orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.refunded": {
		TemplateID:   "ordering/order_refunded",
		EmailSubject: "Your refund has been processed",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_number": data["order_number"],
				"amount":       data["total_amount"],
				"currency":     data["currency"],
				"reason":       data["reason"],
				"order_link":   orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.scheduled": {
		TemplateID:   "ordering/order_scheduled",
		EmailSubject: "Your order has been scheduled",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":          data["customer_name"],
				"order_number":  data["order_number"],
				"scheduled_for": data["scheduled_for"],
				"total_amount":  data["total_amount"],
				"currency":      data["currency"],
				"order_link":    orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.for_pickup": {
		TemplateID:   "ordering/order_for_pickup",
		EmailSubject: "Your order is ready for pickup",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_number": data["order_number"],
				"outlet_name":  data["outlet_name"],
				"pickup_time":  data["pickup_time"],
				"order_link":   orderLink(data, orderAppURL),
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

		evtType := evt.resolvedType()
		evtTenantID := evt.resolvedTenantID()
		evtData := evt.resolvedData()

		mapping, ok := orderMappings[evtType]
		if !ok {
			logg.Debug("order event: unhandled type, skipping", zap.String("type", evtType))
			_ = m.Ack()
			return
		}

		// Extract customer email from event data
		email, _ := evtData["customer_email"].(string)
		if email == "" {
			logg.Warn("order event: no customer_email in data, skipping", zap.String("type", evtType))
			_ = m.Ack()
			return
		}

		// Resolve tenant info for building order links
		tenantWebsite := ""
		tenantSlug := ""
		if ti, err := tr.resolve(ctx, evtTenantID); err == nil {
			tenantWebsite = ti.Website
			tenantSlug = ti.Slug
		} else {
			logg.Warn("order event: could not resolve tenant, using empty website", zap.String("tenant_id", evtTenantID), zap.Error(err))
		}

		// Use ordering app URL for "View Order" links instead of tenant website
		appURL := orderAppBaseURL(tenantSlug, tenantWebsite)

		orderID, _ := evtData["order_id"].(string)

		msg := messaging.Message{
			TenantID:    evtTenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetCustomer,
			To:          []string{email},
			Data:        mapping.DataBuilder(evtData, appURL),
			Metadata: map[string]interface{}{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("order-%s-%s", evtType, orderID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("order event: failed to dispatch notification",
				zap.String("type", evtType),
				zap.String("order_id", orderID),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("order notification dispatched",
			zap.String("type", evtType),
			zap.String("template", mapping.TemplateID),
			zap.String("order_id", orderID),
			zap.String("to", email),
		)
		_ = m.Ack()
	}

	subscribeWithRetry(ctx, js, logg, "order consumer", true, func() (*nats.Subscription, error) {
		return js.Subscribe("ordering.order.>", handler,
			nats.BindStream("ordering"),
			nats.Durable("notifications-ordering-status"),
			nats.ManualAck(),
			nats.AckWait(30*time.Second),
			nats.MaxDeliver(3),
		)
	})
}
