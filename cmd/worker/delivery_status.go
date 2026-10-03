package main

import (
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// deliveryStatusSubject carries the final outcome of a message that named a source document.
const deliveryStatusSubject = "notifications.delivery.status"

// Correlation keys a producer sets in Message.Metadata to hear back how the message went.
const (
	metaSourceService = "source_service"
	metaReferenceType = "reference_type"
	metaReferenceID   = "reference_id"
)

// withDeliveryCorrelation tags a message with the document it is about.
func withDeliveryCorrelation(meta map[string]any, service, refType, refID string) map[string]any {
	if meta == nil {
		meta = map[string]any{}
	}
	if refID != "" {
		meta[metaSourceService], meta[metaReferenceType], meta[metaReferenceID] = service, refType, refID
	}
	return meta
}

// publishDeliveryStatus tells the producing service the final outcome (sent, or failed after the
// fallback provider) of a correlated message. Messages without a correlation publish nothing.
// Best-effort: a failed publish is logged, never retried into a second send.
func publishDeliveryStatus(nc *nats.Conn, msg messaging.Message, status string, deliverErr error, logg *zap.Logger) {
	if nc == nil || msg.Metadata == nil {
		return
	}
	service, _ := msg.Metadata[metaSourceService].(string)
	refType, _ := msg.Metadata[metaReferenceType].(string)
	refID, _ := msg.Metadata[metaReferenceID].(string)
	if service == "" || refType == "" || refID == "" {
		return
	}
	tenantUUID, _ := uuid.Parse(msg.TenantID)
	payload := map[string]any{
		"tenant_id": msg.TenantID, "source_service": service, "reference_type": refType, "reference_id": refID,
		"channel": msg.Channel, "template": msg.TemplateID, "recipients": msg.To, "status": status,
		"request_id": msg.RequestID,
	}
	if deliverErr != nil {
		payload["error"] = deliverErr.Error()
	}
	ev := eventslib.NewEvent("delivery.status", "notification", uuid.New(), tenantUUID, payload)
	data, err := ev.ToJSON()
	if err != nil {
		return
	}
	js, err := nc.JetStream()
	if err != nil {
		logg.Warn("delivery status: jetstream unavailable", zap.Error(err))
		return
	}
	out := nats.NewMsg(deliveryStatusSubject)
	out.Data = data
	out.Header = nats.Header{}
	out.Header.Set("event-id", ev.ID.String())
	if _, err := js.PublishMsg(out); err != nil {
		logg.Warn("delivery status: publish failed", zap.String("reference_id", refID), zap.Error(err))
	}
}
