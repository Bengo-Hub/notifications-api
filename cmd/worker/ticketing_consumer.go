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

// ticketingEvent is the shared-events envelope from ticketing-api.
type ticketingEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

type ticketingNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(payload map[string]any, tenantWebsite string) map[string]any
}

var ticketingMappings = map[string]ticketingNotificationMapping{
	"ticket.assigned": {
		TemplateID:   "ticketing/ticket_assigned",
		EmailSubject: "Ticket assigned to you",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":          "Agent",
				"ticket_id":     payload["ticket_number"],
				"subject":       payload["subject"],
				"priority":      payload["priority"],
				"customer_name": payload["customer_name"],
				"action_link":   fmt.Sprintf("%s/tickets/%s", tenantWebsite, payload["ticket_id"]),
			}
		},
	},
	"ticket.resolved": {
		TemplateID:   "ticketing/ticket_resolved",
		EmailSubject: "Your ticket has been resolved",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":               "Customer",
				"ticket_id":          payload["ticket_number"],
				"subject":            payload["subject"],
				"resolution_summary": payload["resolution_summary"],
				"resolved_by":        payload["resolved_by"],
				"action_link":        fmt.Sprintf("%s/tickets/%s", tenantWebsite, payload["ticket_id"]),
			}
		},
	},
}

// startTicketingConsumer subscribes to ticketing.> events and dispatches
// ticket notification emails.
func startTicketingConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping ticketing consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt ticketingEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("ticketing event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		mapping, ok := ticketingMappings[evt.EventType]
		if !ok {
			logg.Debug("ticketing event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		tenantID := evt.TenantID
		if tenantID == "" {
			logg.Warn("ticketing event: no tenant_id, skipping")
			_ = m.Ack()
			return
		}

		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			logg.Error("ticketing event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		if ti.ContactEmail == "" {
			logg.Warn("ticketing event: tenant has no contact_email, skipping")
			_ = m.Ack()
			return
		}

		// Use agent email from event payload if available, fallback to tenant contact
		recipientEmail := ti.ContactEmail
		if agentEmail, ok := evt.Payload["agent_email"].(string); ok && agentEmail != "" {
			recipientEmail = agentEmail
		}

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetStaff,
			To:          []string{recipientEmail},
			Data:        mapping.DataBuilder(evt.Payload, ti.Website),
			Metadata: map[string]any{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("ticketing-%s-%s", evt.EventType, evt.AggregateID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("ticketing event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}

		logg.Info("ticketing notification dispatched", zap.String("type", evt.EventType), zap.String("template", mapping.TemplateID))
		_ = m.Ack()
	}

	_, err := js.Subscribe("ticketing.>", handler,
		nats.BindStream("ticketing"),
		nats.Durable("notifications-ticketing"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	if err != nil {
		logg.Warn("ticketing consumer subscription failed (ticketing stream may not exist yet)", zap.Error(err))
		return
	}

	logg.Info("ticketing consumer started", zap.String("subject", "ticketing.>"))
}
