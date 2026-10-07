package broadcasts

import (
	"context"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/notifications-api/internal/modules/suppression"
)

// Event subjects this module publishes (the notifications.> stream carries them).
const (
	SubjectCompleted    = "notifications.broadcast.completed"
	SubjectUnsubscribed = "notifications.recipient.unsubscribed"
)

// PublishEvent sends a shared-events envelope (full Event JSON with an event-id header, so
// consumers can dedupe redeliveries). Best-effort: failures are logged.
func PublishEvent(nc *nats.Conn, log *zap.Logger, subject, eventType string, tenantID *uuid.UUID, aggregateID uuid.UUID, payload map[string]any) {
	if nc == nil {
		return
	}
	tid := uuid.Nil
	if tenantID != nil {
		tid = *tenantID
	}
	ev := eventslib.NewEvent(eventType, "notification", aggregateID, tid, payload)
	data, err := ev.ToJSON()
	if err != nil {
		return
	}
	js, err := nc.JetStream()
	if err != nil {
		log.Warn("event publish: jetstream unavailable", zap.String("subject", subject), zap.Error(err))
		return
	}
	msg := nats.NewMsg(subject)
	msg.Data = data
	msg.Header = nats.Header{}
	msg.Header.Set("event-id", ev.ID.String())
	if _, err := js.PublishMsg(msg); err != nil {
		log.Warn("event publish failed", zap.String("subject", subject), zap.Error(err))
	}
}

// UnsubscribeNotifier publishes opt-outs so MarketFlow (which owns customer consent) can clear
// its subscribed flags.
func UnsubscribeNotifier(nc *nats.Conn, log *zap.Logger) suppression.Notifier {
	return func(_ context.Context, ev suppression.UnsubscribedEvent) {
		var tid *uuid.UUID
		if id, err := uuid.Parse(ev.TenantID); err == nil {
			tid = &id
		}
		PublishEvent(nc, log, SubjectUnsubscribed, "recipient.unsubscribed", tid, uuid.New(), map[string]any{
			"tenant_id": ev.TenantID, "channel": ev.Channel, "address_hash": ev.AddressHash, "address": ev.Address,
			"scope": ev.Scope, "reason": ev.Reason, "source": ev.Source,
		})
	}
}
