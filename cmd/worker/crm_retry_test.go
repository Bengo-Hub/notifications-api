package main

import (
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeRetryMsg records how the consumer settled a message.
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

// Before the last delivery a message whose recipient could not be looked up is redelivered
// after crmRetryDelay.
func TestRetryCRMLookupNaksBeforeTheLastDelivery(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	m := &fakeRetryMsg{delivered: 1}

	if gaveUp := retryCRMLookup(m, zap.New(core), "pos", "loyalty.points.earned", "t1", "c1"); gaveUp {
		t.Fatal("must not give up on the first delivery")
	}
	if !m.naked || m.nakDelay != crmRetryDelay || m.acked {
		t.Fatalf("naked=%v delay=%v acked=%v, want a delayed nak", m.naked, m.nakDelay, m.acked)
	}
	if logs.FilterMessageSnippet("gave up").Len() != 0 {
		t.Fatal("no gave-up line before the last delivery")
	}
}

// On the last delivery the message is acknowledged and dropped with one "gave up" line that
// carries ids only.
func TestRetryCRMLookupGivesUpOnTheLastDelivery(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	m := &fakeRetryMsg{delivered: crmRetryMaxDeliver}

	if gaveUp := retryCRMLookup(m, zap.New(core), "library", "hold.ready", "t1", "c1"); !gaveUp {
		t.Fatal("must give up on the last delivery")
	}
	if !m.acked || m.naked {
		t.Fatalf("acked=%v naked=%v, want an ack", m.acked, m.naked)
	}
	lines := logs.FilterMessageSnippet("gave up after max deliveries").All()
	if len(lines) != 1 {
		t.Fatalf("got %d gave-up lines, want 1", len(lines))
	}
	for k := range lines[0].ContextMap() {
		switch k {
		case "consumer", "type", "tenant_id", "crm_contact_id", "deliveries":
		default:
			t.Errorf("unexpected log field %q (ids only)", k)
		}
	}
}

// Without JetStream metadata the delivery count is unknown: retry, the server still enforces
// MaxDeliver.
func TestRetryCRMLookupWithoutMetadataRetries(t *testing.T) {
	m := &fakeRetryMsg{mdErr: errors.New("not a jetstream message")}
	if retryCRMLookup(m, zap.NewNop(), "pos", "hotel.check_in", "t1", "c1") || !m.naked {
		t.Fatal("want a retry")
	}
}
