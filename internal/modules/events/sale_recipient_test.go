package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/bengobox/notifications-api/internal/contacts"
)

// fakeLookup answers like contacts.Resolver.Lookup: each field from the CRM when it has one,
// else from the fallback.
type fakeLookup struct {
	rec   contacts.Recipient
	err   error
	calls int
}

func (f *fakeLookup) Lookup(_ context.Context, _, _ uuid.UUID, fallback contacts.Recipient) (contacts.Recipient, error) {
	f.calls++
	if f.err != nil {
		return fallback, f.err
	}
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	return contacts.Recipient{Name: pick(f.rec.Name, fallback.Name), Email: pick(f.rec.Email, fallback.Email), Phone: pick(f.rec.Phone, fallback.Phone)}, nil
}

// A receipt event for a CRM-linked customer carries crm_contact_id only: the name, phone and
// email come from the CRM.
func TestSaleRecipientLinkedComesFromTheCRM(t *testing.T) {
	crm := &fakeLookup{rec: contacts.Recipient{Name: "Jane", Email: "jane@example.com", Phone: "+254712345678"}}
	got, err := saleRecipient(context.Background(), crm, uuid.NewString(), uuid.NewString(), contacts.Recipient{})
	if err != nil || got != crm.rec {
		t.Fatalf("got %+v, %v; want the CRM recipient", got, err)
	}
}

// A phone or email the cashier typed for this send wins over the CRM; the CRM fills the rest.
func TestSaleRecipientTypedDestinationWins(t *testing.T) {
	crm := &fakeLookup{rec: contacts.Recipient{Name: "Jane", Email: "jane@example.com", Phone: "+254712345678"}}
	got, err := saleRecipient(context.Background(), crm, uuid.NewString(), uuid.NewString(), contacts.Recipient{Email: "typed@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := contacts.Recipient{Name: "Jane", Email: "typed@example.com", Phone: "+254712345678"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// An unlinked customer's event carries their details, and the CRM is not asked.
func TestSaleRecipientUnlinkedUsesTheEvent(t *testing.T) {
	crm := &fakeLookup{rec: contacts.Recipient{Name: "Other"}}
	typed := contacts.Recipient{Name: "Ann", Phone: "0711111111"}
	got, err := saleRecipient(context.Background(), crm, uuid.NewString(), "", typed)
	if err != nil || got != typed || crm.calls != 0 {
		t.Fatalf("got %+v, %v, calls=%d; want the event's fields and no CRM call", got, err, crm.calls)
	}
}

// When the CRM cannot be reached the error is reported so the handler can retry.
func TestSaleRecipientCRMDownReportsTheError(t *testing.T) {
	crm := &fakeLookup{err: errors.New("crm down")}
	got, err := saleRecipient(context.Background(), crm, uuid.NewString(), uuid.NewString(), contacts.Recipient{})
	if err == nil || got.Email != "" || got.Phone != "" {
		t.Fatalf("got %+v, %v; want an error and nothing to send to", got, err)
	}
}

// fakeRetryMsg records how the handler settled a message.
type fakeRetryMsg struct {
	delivered uint64
	mdErr     error
	nakDelay  time.Duration
	naked     bool
	acked     bool
}

func (f *fakeRetryMsg) Metadata() (*nats.MsgMetadata, error) {
	if f.mdErr != nil {
		return nil, f.mdErr
	}
	return &nats.MsgMetadata{NumDelivered: f.delivered}, nil
}

func (f *fakeRetryMsg) NakWithDelay(d time.Duration, _ ...nats.AckOpt) error {
	f.naked, f.nakDelay = true, d
	return nil
}

func (f *fakeRetryMsg) Ack(_ ...nats.AckOpt) error {
	f.acked = true
	return nil
}

// Before the durable's last delivery a receipt whose customer could not be looked up is
// redelivered later.
func TestRetrySaleCRMLookupNaksBeforeTheLastDelivery(t *testing.T) {
	m := &fakeRetryMsg{delivered: 1}
	if retrySaleCRMLookup(m, zap.NewNop(), "t1", "c1") {
		t.Fatal("must not give up on the first delivery")
	}
	if !m.naked || m.nakDelay != saleCRMRetryDelay || m.acked {
		t.Fatalf("naked=%v delay=%v acked=%v, want a delayed nak", m.naked, m.nakDelay, m.acked)
	}
}

// On the last delivery the receipt is dropped with one "gave up" line carrying ids only.
func TestRetrySaleCRMLookupGivesUpOnTheLastDelivery(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	m := &fakeRetryMsg{delivered: maxDeliver}
	if !retrySaleCRMLookup(m, zap.New(core), "t1", "c1") {
		t.Fatal("must give up on the last delivery")
	}
	if !m.acked || m.naked {
		t.Fatalf("acked=%v naked=%v, want an ack", m.acked, m.naked)
	}
	lines := logs.FilterMessageSnippet("gave up").All()
	if len(lines) != 1 {
		t.Fatalf("got %d gave-up lines, want 1", len(lines))
	}
	for k := range lines[0].ContextMap() {
		switch k {
		case "tenant_id", "crm_contact_id", "deliveries":
		default:
			t.Errorf("unexpected log field %q (ids only)", k)
		}
	}
}

// Without JetStream metadata the count is unknown: retry, the server still stops at MaxDeliver.
func TestRetrySaleCRMLookupWithoutMetadataRetries(t *testing.T) {
	m := &fakeRetryMsg{mdErr: errors.New("not a jetstream message")}
	if retrySaleCRMLookup(m, zap.NewNop(), "t1", "c1") || !m.naked {
		t.Fatal("want a retry")
	}
}
