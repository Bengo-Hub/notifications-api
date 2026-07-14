package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// inventoryEvent is the CloudEvents envelope from inventory-service.
type inventoryEvent struct {
	ID       string                 `json:"id"`
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      string                 `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// inventoryNotificationMapping maps event types to notification details.
type inventoryNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	// ToBuyer routes the email to the customer (payload buyer_email) instead of the tenant admin.
	ToBuyer     bool
	DataBuilder func(data map[string]interface{}, tenantWebsite string) map[string]interface{}
}

var inventoryMappings = map[string]inventoryNotificationMapping{
	"inventory.stock.low": {
		TemplateID:   "inventory/low_stock_alert",
		EmailSubject: "Low Stock Alert",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":          "Store Manager",
				"item_name":     data["name"],
				"item_sku":      data["sku"],
				"current_stock": data["available"],
				"min_threshold": data["reorder_level"],
				"unit":          "",
				"location":      data["warehouse_id"],
				"item_link":     fmt.Sprintf("%s/dashboard/inventory?sku=%s", tenantWebsite, data["sku"]),
			}
		},
	},
	"inventory.stock.out": {
		TemplateID:   "inventory/stock_out",
		EmailSubject: "URGENT: Stock Out Alert",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":      "Store Manager",
				"item_name": data["name"],
				"item_sku":  data["sku"],
				"location":  data["warehouse_id"],
				"item_link": fmt.Sprintf("%s/dashboard/inventory?sku=%s", tenantWebsite, data["sku"]),
			}
		},
	},
	// Event ticket issued — delivered to the buyer with their ticket code.
	"inventory.ticket.issued": {
		TemplateID:   "events/ticket_issued",
		EmailSubject: "Your event ticket",
		ToBuyer:      true,
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			buyer, _ := data["buyer_name"].(string)
			if buyer == "" {
				buyer = "there"
			}
			return map[string]interface{}{
				"buyer_name":   buyer,
				"code":         data["code"],
				"tier_name":    data["tier_name"],
				"quantity":     data["quantity"],
				"currency":    data["currency"],
				"total_price": data["total_price"],
				"ticket_link": "",
			}
		},
	},
}

// startInventoryConsumer subscribes to inventory.> events from the inventory-service
// JetStream stream and republishes them as notification messages.
func startInventoryConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping inventory consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt inventoryEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("inventory event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		mapping, ok := inventoryMappings[evt.AggregateType + "." + evt.EventType]
		if !ok {
			logg.Debug("inventory event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		// Resolve tenant admin email and website from local tenant table
		ti, err := tr.resolve(ctx, evt.TenantID)
		if err != nil {
			logg.Error("inventory event: failed to resolve tenant", zap.String("tenant_id", evt.TenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		// Staff alert emails (low stock / stock out) are OPT-IN: the publisher embeds
		// notification.enabled from the tenant's inventory config. Absent or false means
		// the tenant never opted in — skip. Buyer-facing messages (tickets) are
		// transactional and always send.
		notif, _ := evt.Payload["notification"].(map[string]interface{})
		if !mapping.ToBuyer {
			if enabled, _ := notif["enabled"].(bool); !enabled {
				logg.Debug("inventory event: alert emails not enabled for tenant, skipping",
					zap.String("tenant_id", evt.TenantID), zap.String("type", evt.EventType))
				_ = m.Ack()
				return
			}
		}

		// Recipient: customer-facing notifications (e.g. ticket issued) go to the buyer; staff
		// alerts go to the tenant's configured alert address, falling back to the tenant admin.
		recipient := ti.ContactEmail
		if alertEmail, _ := notif["email"].(string); alertEmail != "" && !mapping.ToBuyer {
			recipient = alertEmail
		}
		target := messaging.TargetStaff
		if mapping.ToBuyer {
			buyerEmail, _ := evt.Payload["buyer_email"].(string)
			if buyerEmail == "" {
				logg.Warn("inventory event: ticket has no buyer_email, skipping", zap.String("tenant_id", evt.TenantID))
				_ = m.Ack()
				return
			}
			recipient = buyerEmail
			target = messaging.TargetCustomer
		}
		if recipient == "" {
			logg.Warn("inventory event: no recipient, skipping", zap.String("tenant_id", evt.TenantID))
			_ = m.Ack()
			return
		}

		sku, _ := evt.Payload["sku"].(string)

		data := mapping.DataBuilder(evt.Payload, ti.Website)
		// For buyer-facing tickets, link to the public branded ticket PDF (with QR) on inventory-api.
		if mapping.ToBuyer {
			if code, _ := evt.Payload["code"].(string); code != "" && ti.Slug != "" {
				invBase := ti.ServiceURL("inventory_api", "NOTIFICATIONS_INVENTORY_API_URL", "")
				if invBase != "" {
					data["ticket_link"] = fmt.Sprintf("%s/api/v1/%s/inventory/tickets/%s/pdf", strings.TrimRight(invBase, "/"), ti.Slug, code)
				}
			}
		}

		msg := messaging.Message{
			TenantID:    evt.TenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      target,
			To:          []string{recipient},
			Data:        data,
			Metadata: map[string]interface{}{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("inventory-%s-%s-%s", evt.EventType, sku, evt.ID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("inventory event: failed to dispatch notification",
				zap.String("type", evt.EventType),
				zap.String("sku", sku),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("inventory notification dispatched",
			zap.String("type", evt.EventType),
			zap.String("template", mapping.TemplateID),
			zap.String("sku", sku),
		)
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "inventory", "inventory.>", "notifications-inventory-stock", handler,
		nats.BindStream("inventory"),
		nats.Durable("notifications-inventory-stock"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
