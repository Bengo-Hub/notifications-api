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

// projectsEvent is the shared-events envelope from projects-api.
type projectsEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

type projectsNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(payload map[string]any, tenantWebsite string) map[string]any
}

var projectsMappings = map[string]projectsNotificationMapping{
	"milestone.reached": {
		TemplateID:   "projects/project_milestone_reached",
		EmailSubject: "Project milestone reached",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":            "Team",
				"project_name":    payload["project_name"],
				"milestone_name":  payload["milestone_name"],
				"completion_date": payload["completion_date"],
				"progress":        payload["progress"],
				"action_link":     fmt.Sprintf("%s/projects/%s", tenantWebsite, payload["project_id"]),
			}
		},
	},
}

// startProjectsConsumer subscribes to project.> events and dispatches
// project notification emails.
func startProjectsConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping projects consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt projectsEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("projects event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		mapping, ok := projectsMappings[evt.EventType]
		if !ok {
			logg.Debug("projects event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		tenantID := evt.TenantID
		if tenantID == "" {
			logg.Warn("projects event: no tenant_id, skipping")
			_ = m.Ack()
			return
		}

		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			logg.Error("projects event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		if ti.ContactEmail == "" {
			logg.Warn("projects event: tenant has no contact_email, skipping")
			_ = m.Ack()
			return
		}

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetStaff,
			To:          []string{ti.ContactEmail},
			Data:        mapping.DataBuilder(evt.Payload, ti.Website),
			Metadata: map[string]any{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("projects-%s-%s", evt.EventType, evt.AggregateID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("projects event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}

		logg.Info("projects notification dispatched", zap.String("type", evt.EventType), zap.String("template", mapping.TemplateID))
		_ = m.Ack()
	}

	// Projects-api stream name — check what's configured
	eventslib.SubscribeWithRebind(logg, js, "project.>", handler,
		nats.BindStream("projects"),
		nats.Durable("notifications-projects"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
