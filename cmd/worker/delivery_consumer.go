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

// deliveryEvent is the CloudEvents envelope from logistics-service task events.
type deliveryEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenantId"`
	Data     map[string]interface{} `json:"data"`
}

// deliveryNotificationMapping maps task event types to notification details.
type deliveryNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(data map[string]interface{}, tenantWebsite string) map[string]interface{}
}

var deliveryMappings = map[string]deliveryNotificationMapping{
	"logistics.task.assigned": {
		TemplateID:   "email/logistics/delivery_assigned",
		EmailSubject: "Your delivery has been assigned",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			trackingCode, _ := data["tracking_code"].(string)
			return map[string]interface{}{
				"name":          "Customer",
				"order_id":      data["external_reference"],
				"driver_name":   data["fleet_member_id"],
				"tracking_link": fmt.Sprintf("%s/track/%s", tenantWebsite, trackingCode),
			}
		},
	},
	"logistics.task.delivered": {
		TemplateID:   "email/logistics/delivery_completed",
		EmailSubject: "Your delivery has been completed",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       "Customer",
				"order_id":   data["external_reference"],
				"order_link": fmt.Sprintf("%s/orders", tenantWebsite),
			}
		},
	},
	"logistics.task.failed": {
		TemplateID:   "email/logistics/delivery_failed",
		EmailSubject: "Delivery attempt failed",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       "Customer",
				"order_id":   data["external_reference"],
				"order_link": fmt.Sprintf("%s/orders", tenantWebsite),
			}
		},
	},
}

// startDeliveryConsumer subscribes to logistics.task.> events and dispatches
// delivery notification emails. This is separate from the fleet consumer which
// handles logistics.fleet.> events for rider lifecycle notifications.
func startDeliveryConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping delivery consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt deliveryEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("delivery event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		mapping, ok := deliveryMappings[evt.Type]
		if !ok {
			logg.Debug("delivery event: unhandled type, skipping", zap.String("type", evt.Type))
			_ = m.Ack()
			return
		}

		// Resolve tenant contact email for delivery notifications
		ti, err := tr.resolve(ctx, evt.TenantID)
		if err != nil {
			logg.Error("delivery event: failed to resolve tenant", zap.String("tenant_id", evt.TenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		if ti.ContactEmail == "" {
			logg.Warn("delivery event: tenant has no contact_email, skipping", zap.String("tenant_id", evt.TenantID))
			_ = m.Ack()
			return
		}

		tenantWebsite := ti.Website

		taskID, _ := evt.Data["task_id"].(string)

		// Use customer email from event if available, fallback to tenant contact
		recipientEmail := ti.ContactEmail
		if ce, ok := evt.Data["customer_email"].(string); ok && ce != "" {
			recipientEmail = ce
		}

		msg := messaging.Message{
			TenantID:    evt.TenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetCustomer,
			To:          []string{recipientEmail},
			Data:        mapping.DataBuilder(evt.Data, tenantWebsite),
			Metadata: map[string]interface{}{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("delivery-%s-%s", evt.Type, taskID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("delivery event: failed to dispatch notification",
				zap.String("type", evt.Type),
				zap.String("task_id", taskID),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("delivery notification dispatched",
			zap.String("type", evt.Type),
			zap.String("template", mapping.TemplateID),
			zap.String("task_id", taskID),
		)
		_ = m.Ack()
	}

	_, err := js.Subscribe("logistics.task.>", handler,
		nats.BindStream("logistics"),
		nats.Durable("notifications-logistics-delivery"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	if err != nil {
		logg.Warn("delivery consumer subscription failed (logistics stream may not exist yet)", zap.Error(err))
		return
	}

	logg.Info("delivery consumer started", zap.String("subject", "logistics.task.>"))
}
