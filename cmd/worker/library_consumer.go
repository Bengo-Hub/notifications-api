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

// libraryEvent is the outbox envelope from library-api (shared-events).
type libraryEvent struct {
	ID            string                 `json:"id"`
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      string                 `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

type libraryNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(data map[string]interface{}, tenantWebsite string) map[string]interface{}
}

// Keyed by {aggregate_type}.{event_type} — aggregate_type is always "library".
var libraryMappings = map[string]libraryNotificationMapping{
	"library.loan.overdue": {
		TemplateID:   "library/overdue_notice",
		EmailSubject: "Library item overdue",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":       "Librarian",
				"member_id":  data["member_id"],
				"loan_id":    data["loan_id"],
				"due_at":     data["due_at"],
				"dashboard":  fmt.Sprintf("%s/circulation", tenantWebsite),
			}
		},
	},
	"library.hold.ready": {
		TemplateID:   "library/hold_ready",
		EmailSubject: "A reserved item is ready for pickup",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":        "Librarian",
				"member_id":   data["member_id"],
				"bib_record":  data["bib_record_id"],
				"expires_at":  data["expires_at"],
				"dashboard":   fmt.Sprintf("%s/holds", tenantWebsite),
			}
		},
	},
	"library.fine.assessed": {
		TemplateID:   "library/fine_assessed",
		EmailSubject: "A fine was added to a member account",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":      "Librarian",
				"member_id": data["member_id"],
				"amount":    data["amount"],
				"reason":    data["reason"],
				"dashboard": fmt.Sprintf("%s/fines", tenantWebsite),
			}
		},
	},
	"library.member.registered": {
		TemplateID:   "library/member_welcome",
		EmailSubject: "New library member registered",
		DataBuilder: func(data map[string]interface{}, tenantWebsite string) map[string]interface{} {
			return map[string]interface{}{
				"name":          "Librarian",
				"member_name":   data["display_name"],
				"membership_no": data["membership_no"],
				"dashboard":     fmt.Sprintf("%s/members", tenantWebsite),
			}
		},
	},
}

// startLibraryConsumer subscribes to library.> events and republishes them as
// notification messages (routed to the tenant librarian / admin).
func startLibraryConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping library consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt libraryEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("library event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		mapping, ok := libraryMappings[evt.AggregateType+"."+evt.EventType]
		if !ok {
			_ = m.Ack()
			return
		}
		ti, err := tr.resolve(ctx, evt.TenantID)
		if err != nil {
			logg.Error("library event: failed to resolve tenant", zap.String("tenant_id", evt.TenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		// Patron-direct when the event carries a member email; else fall back to the librarian.
		recipient := ti.ContactEmail
		target := messaging.TargetStaff
		if memberEmail, _ := evt.Payload["email"].(string); memberEmail != "" {
			recipient = memberEmail
			target = messaging.TargetCustomer
		}
		if recipient == "" {
			_ = m.Ack()
			return
		}
		data := mapping.DataBuilder(evt.Payload, ti.Website)
		if n, _ := evt.Payload["name"].(string); n != "" {
			data["name"] = n // greet the actual member when known
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
			IdempotencyKey: fmt.Sprintf("library-%s-%s", evt.EventType, evt.ID),
			QueuedAt:       time.Now(),
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("library event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}
		logg.Info("library notification dispatched", zap.String("type", evt.EventType), zap.String("template", mapping.TemplateID))
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "library", "library.>", "notifications-library", handler,
		nats.BindStream("library"),
		nats.Durable("notifications-library"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
