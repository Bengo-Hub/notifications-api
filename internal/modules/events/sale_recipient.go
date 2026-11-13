package events

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/contacts"
)

// contactLookup is the part of contacts.Resolver the receipt handler uses.
type contactLookup interface {
	Lookup(ctx context.Context, tenantID, contactID uuid.UUID, fallback contacts.Recipient) (contacts.Recipient, error)
}

// saleCRMRetryDelay is how long a receipt waits before it is redelivered while the CRM cannot be
// reached. The retry count is the durable's own maxDeliver: nats.go refuses to bind to an
// existing durable whose MaxDeliver differs, so it is not changed here.
const saleCRMRetryDelay = 30 * time.Second

// needsCRMRecipient reports whether a sale receipt event's recipient must be read from the CRM.
// A receipt request with a contact id always asks (the name comes from the CRM). The automatic
// pos.sale.finalized receipt asks only when the event names a linked contact and carries no phone
// or email (pos-api sends ids only for linked customers, S2-8 I-4), so an anonymous or unlinked
// sale never costs a CRM call.
func needsCRMRecipient(subject, contactID, phone, email string) bool {
	if contacts.ParseID(contactID) == uuid.Nil {
		return false
	}
	switch subject {
	case "pos.sale.notification_requested":
		return true
	case "pos.sale.finalized":
		return phone == "" && email == ""
	}
	return false
}

// saleRecipient returns who a sale receipt goes to (a receipt request, or the automatic receipt
// of an ids-only pos.sale.finalized).
//
// pos-api sends crm_contact_id alone for a CRM-linked customer, plus customer_phone or
// customer_email only when the cashier typed one for this send. typed holds whatever the event
// carried. With no contact id (an unlinked customer) typed is returned unchanged and the CRM is
// not asked. With one, the name comes from the CRM, and a typed phone or email wins over the
// CRM's. The error is set when the CRM could not answer.
func saleRecipient(ctx context.Context, res contactLookup, tenantID, contactID string, typed contacts.Recipient) (contacts.Recipient, error) {
	cid := contacts.ParseID(contactID)
	if res == nil || cid == uuid.Nil {
		return typed, nil
	}
	crm, err := res.Lookup(ctx, contacts.ParseID(tenantID), cid, contacts.Recipient{})
	return contacts.Recipient{
		Name:  firstNonBlank(crm.Name, typed.Name),
		Email: firstNonBlank(typed.Email, crm.Email),
		Phone: firstNonBlank(typed.Phone, crm.Phone),
	}, err
}

func firstNonBlank(a, b string) string {
	if a = strings.TrimSpace(a); a != "" {
		return a
	}
	return strings.TrimSpace(b)
}

// retryMsg is the part of *nats.Msg the retry decision uses, so tests can drive it.
type retryMsg interface {
	Metadata() (*nats.MsgMetadata, error)
	NakWithDelay(delay time.Duration, opts ...nats.AckOpt) error
	Ack(opts ...nats.AckOpt) error
}

// retrySaleCRMLookup settles a receipt whose linked customer could not be looked up because the
// CRM was unreachable and the event had no phone or email of its own. It follows the worker's
// pos and library consumers (cmd/worker/crm_retry.go): before the last delivery it asks for a
// redelivery after saleCRMRetryDelay; on the last delivery it acknowledges the message, logs one
// "gave up" line (ids only) and returns true. Without JetStream metadata it retries; the server
// still stops at maxDeliver.
func retrySaleCRMLookup(m retryMsg, logg *zap.Logger, tenantID, contactID string) bool {
	if md, err := m.Metadata(); err == nil && md.NumDelivered >= maxDeliver {
		logg.Warn("sale receipt dropped: CRM unreachable, gave up after max deliveries",
			zap.String("tenant_id", tenantID), zap.String("crm_contact_id", contactID),
			zap.Uint64("deliveries", md.NumDelivered))
		_ = m.Ack()
		return true
	}
	logg.Warn("sale receipt: CRM unreachable and the event has no contact details, retrying later",
		zap.String("tenant_id", tenantID), zap.String("crm_contact_id", contactID))
	_ = m.NakWithDelay(saleCRMRetryDelay)
	return false
}
