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

// fleetEvent matches the shared-events Event struct JSON schema.
type fleetEvent struct {
	ID            string                 `json:"id"`
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      string                 `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// Subject reconstructs the NATS subject: {aggregate_type}.{event_type}
func (e fleetEvent) Subject() string {
	return e.AggregateType + "." + e.EventType
}

// fleetNotificationMapping maps event types to notification details.
type fleetNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(data map[string]interface{}, tenantWebsite string) map[string]interface{}
}

var fleetMappings = map[string]fleetNotificationMapping{
	"logistics.fleet.member_invited": {
		TemplateID:   "logistics/rider_invite",
		EmailSubject: "You've been invited to join the fleet",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"RiderName":    data["user_name"],
				"DashboardUrl": fmt.Sprintf("%s/dashboard", tenantWebsite),
			}
		},
	},
	"logistics.fleet.member_approved": {
		TemplateID:   "logistics/rider_onboarding_approved",
		EmailSubject: "Your rider application has been approved",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"RiderName":    data["user_name"],
				"DashboardUrl": fmt.Sprintf("%s/dashboard", tenantWebsite),
			}
		},
	},
	"logistics.fleet.member_suspended": {
		TemplateID:   "logistics/rider_suspended",
		EmailSubject: "Your fleet membership has been suspended",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"RiderName":  data["user_name"],
				"SupportUrl": fmt.Sprintf("%s/support", tenantWebsite),
			}
		},
	},
}

// startFleetConsumer subscribes to logistics.fleet.> events from the logistics
// JetStream stream and republishes them as notification messages.
func startFleetConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping fleet consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt fleetEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("fleet event: unmarshal failed", zap.Error(err))
			_ = m.Ack() // unrecoverable
			return
		}

		mapping, ok := fleetMappings[evt.Subject()]
		if !ok {
			logg.Debug("fleet event: unhandled type, skipping", zap.String("type", evt.Subject()))
			_ = m.Ack()
			return
		}

		// Extract recipient email from event data
		email, _ := evt.Payload["user_email"].(string)
		if email == "" {
			logg.Warn("fleet event: no user_email in data, skipping", zap.String("type", evt.Subject()))
			_ = m.Ack()
			return
		}

		// Resolve tenant website for building dashboard/support links
		tenantWebsite := ""
		if ti, err := tr.resolve(ctx, evt.TenantID); err == nil {
			tenantWebsite = ti.Website
		} else {
			logg.Warn("fleet event: could not resolve tenant, using empty website", zap.String("tenant_id", evt.TenantID), zap.Error(err))
		}

		memberID, _ := evt.Payload["member_id"].(string)

		// Build notification message
		msg := messaging.Message{
			TenantID:    evt.TenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetRider,
			To:          []string{email},
			Data:        mapping.DataBuilder(evt.Payload, tenantWebsite),
			Metadata: map[string]interface{}{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("fleet-%s-%s", evt.Subject(), memberID),
			QueuedAt:       time.Now(),
		}

		// Publish to notifications stream for the existing worker to process
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("fleet event: failed to dispatch notification",
				zap.String("type", evt.Subject()),
				zap.String("email", email),
				zap.Error(err),
			)
			_ = m.Nak() // retry
			return
		}

		logg.Info("fleet notification dispatched",
			zap.String("type", evt.Subject()),
			zap.String("template", mapping.TemplateID),
			zap.String("to", email),
		)
		_ = m.Ack()
	}

	_, err := js.Subscribe("logistics.fleet.>", handler,
		nats.BindStream("logistics"),
		nats.Durable("notifications-logistics-fleet"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	if err != nil {
		// Non-fatal: logistics stream may not exist yet if logistics-api hasn't started
		logg.Warn("fleet consumer subscription failed (logistics stream may not exist yet)", zap.Error(err))
		return
	}

	logg.Info("fleet consumer started", zap.String("subject", "logistics.fleet.>"))
}
