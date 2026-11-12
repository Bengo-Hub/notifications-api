package main

import (
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// A notice whose recipient comes from the CRM (the event carries crm_contact_id and no email or
// phone of its own) is retried while the CRM cannot be reached, then dropped. The pos and library
// consumers subscribe with nats.MaxDeliver(crmRetryMaxDeliver), so the server stops redelivering
// at the same count this code treats as the last delivery.
//
// nats.go refuses to bind to an existing durable whose MaxDeliver differs from the one asked for,
// so changing crmRetryMaxDeliver also needs the durable (notifications-pos-orders on the pos
// stream, notifications-library on the library stream) edited or deleted once on the server,
// otherwise the consumer does not start.
const (
	crmRetryDelay      = 30 * time.Second
	crmRetryMaxDeliver = 3
)

// retryMsg is the part of *nats.Msg the retry decision uses, so tests can drive it.
type retryMsg interface {
	Metadata() (*nats.MsgMetadata, error)
	NakWithDelay(delay time.Duration, opts ...nats.AckOpt) error
	Ack(opts ...nats.AckOpt) error
}

// retryCRMLookup settles a message whose recipient could not be looked up because the CRM was
// unreachable. Before the last delivery it asks for a redelivery after crmRetryDelay. On the last
// delivery it acknowledges the message, logs one "gave up" line (ids only) and returns true.
// Without JetStream metadata the count is unknown and it retries; the server still stops at
// MaxDeliver.
func retryCRMLookup(m retryMsg, logg *zap.Logger, consumer, eventType, tenantID, contactID string) bool {
	if md, err := m.Metadata(); err == nil && md.NumDelivered >= crmRetryMaxDeliver {
		logg.Warn("notice dropped: CRM unreachable, gave up after max deliveries",
			zap.String("consumer", consumer), zap.String("type", eventType),
			zap.String("tenant_id", tenantID), zap.String("crm_contact_id", contactID),
			zap.Uint64("deliveries", md.NumDelivered))
		_ = m.Ack()
		return true
	}
	logg.Warn("CRM unreachable and the event has no contact details, retrying later",
		zap.String("consumer", consumer), zap.String("type", eventType),
		zap.String("tenant_id", tenantID), zap.String("crm_contact_id", contactID))
	_ = m.NakWithDelay(crmRetryDelay)
	return false
}
