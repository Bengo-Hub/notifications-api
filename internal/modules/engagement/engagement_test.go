package engagement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
)

type memStore map[string]string

func (m memStore) Put(_ context.Context, k, v string, _ time.Duration) error { m[k] = v; return nil }
func (m memStore) Get(_ context.Context, k string) (string, error) {
	v, ok := m[k]
	if !ok {
		return "", errors.New("nil")
	}
	return v, nil
}
func (m memStore) Take(ctx context.Context, k string) (string, error) {
	v, err := m.Get(ctx, k)
	delete(m, k)
	return v, err
}

type published struct {
	subject string
	payload map[string]any
}

func newTracker() (*Tracker, memStore, *[]published) {
	store := memStore{}
	var out []published
	t := NewWith(store, func(subject, _ string, _ *uuid.UUID, payload map[string]any) {
		out = append(out, published{subject, payload})
	}, zap.NewNop())
	return t, store, &out
}

func nurtureMessage(channel string) messaging.Message {
	return messaging.Message{
		TenantID:  uuid.NewString(),
		Channel:   channel,
		To:        []string{"+254712345678"},
		RequestID: "req-1",
		Metadata: map[string]any{
			"source": "marketflow-worker", "enrollment_id": uuid.NewString(), "sequence_id": "seq-1", "step_index": "2",
		},
	}
}

func TestNonNurtureMessagesPublishNothing(t *testing.T) {
	tr, store, out := newTracker()
	tr.MessageSent(context.Background(), messaging.Message{TenantID: uuid.NewString(), Channel: "email", To: []string{"a@b.co"}})
	tr.MessageFailed(messaging.Message{TenantID: "slug", Channel: "sms"}, errors.New("x"))
	if len(*out) != 0 || len(store) != 0 {
		t.Fatalf("published %v, stored %v", *out, store)
	}
	var nilTracker *Tracker
	nilTracker.MessageSent(context.Background(), nurtureMessage("email"))
	nilTracker.WhatsAppStatus(context.Background(), "wamid", "read")
	nilTracker.Replied(context.Background(), "whatsapp", "0712345678", "", "m")
}

func TestWhatsAppSentReadAndReply(t *testing.T) {
	ctx := context.Background()
	tr, _, out := newTracker()
	msg := nurtureMessage("whatsapp")
	msg.Metadata[messaging.MetaSentMessageID] = "wamid.1"
	tr.MessageSent(ctx, msg)
	tr.WhatsAppStatus(ctx, "wamid.1", "sent") // already reported
	tr.WhatsAppStatus(ctx, "wamid.1", "delivered")
	tr.WhatsAppStatus(ctx, "wamid.1", "read")
	tr.WhatsAppStatus(ctx, "wamid.other", "read")
	tr.Replied(ctx, "whatsapp", "0712345678", uuid.NewString(), "in-0") // another tenant's number
	tr.Replied(ctx, "whatsapp", "254712345678", msg.TenantID, "in-1")
	tr.Replied(ctx, "whatsapp", "254712345678", msg.TenantID, "in-2") // only the first reply counts

	want := []string{Sent, Delivered, Opened, Replied}
	if len(*out) != len(want) {
		t.Fatalf("want %v, got %+v", want, *out)
	}
	for i, p := range *out {
		if p.subject != Subject || p.payload["event_type"] != want[i] {
			t.Fatalf("event %d: %+v", i, p)
		}
		if p.payload["enrollment_id"] != msg.Metadata["enrollment_id"] || p.payload["step_index"] != 2 || p.payload["tenant_id"] != msg.TenantID {
			t.Fatalf("correlation lost: %+v", p.payload)
		}
	}
}

func TestEmailBounceAndFailure(t *testing.T) {
	ctx := context.Background()
	tr, _, out := newTracker()
	msg := nurtureMessage("email")
	tr.MessageSent(ctx, msg)
	tr.EmailBounced(ctx, "req-1")
	tr.EmailBounced(ctx, "req-1") // a resent webhook batch
	tr.EmailBounced(ctx, "")
	tr.MessageFailed(msg, errors.New("smtp down"))
	got := []string{}
	for _, p := range *out {
		got = append(got, p.payload["event_type"].(string))
	}
	if len(got) != 3 || got[0] != Sent || got[1] != Bounced || got[2] != Failed {
		t.Fatalf("events %v", got)
	}
}
