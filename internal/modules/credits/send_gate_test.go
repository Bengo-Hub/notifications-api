package credits

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

func TestSendGateBlocksUnentitledTenantsAndCachesTheAnswer(t *testing.T) {
	blocked, allowed := uuid.New(), uuid.New()
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("tenant_id") == blocked.String() {
			_, _ = w.Write([]byte(`{"allowed":false,"reason":"service_not_activated"}`))
			return
		}
		_, _ = w.Write([]byte(`{"allowed":true}`))
	})
	resolve := func(_ context.Context, s string) (uuid.UUID, bool) {
		if s == "shop" {
			return blocked, true
		}
		id, err := uuid.Parse(s)
		return id, err == nil
	}
	g := NewSendGate(c, resolve, time.Minute, zap.NewNop())
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	ctx := context.Background()

	// A slug resolves to the tenant; the refusal names what to buy.
	if r := g.Blocked(ctx, messaging.Message{TenantID: "shop", Channel: "whatsapp"}); r == "" {
		t.Fatal("a tenant without WhatsApp must be blocked at the source")
	}
	for i := 0; i < 5; i++ {
		g.Blocked(ctx, messaging.Message{TenantID: blocked.String(), Channel: "whatsapp"})
	}
	if calls.Load() != 1 {
		t.Fatalf("answers must be cached per tenant and service: %d calls", calls.Load())
	}
	if r := g.Blocked(ctx, messaging.Message{TenantID: allowed.String(), Channel: "sms"}); r != "" {
		t.Fatalf("an entitled tenant was blocked: %q", r)
	}
	// Not paid or not the tenant's to pay: never asked.
	before := calls.Load()
	for _, m := range []messaging.Message{
		{TenantID: blocked.String(), Channel: "email"},
		{TenantID: blocked.String(), Channel: "push"},
		{TenantID: blocked.String(), Channel: "whatsapp", SenderScope: messaging.SenderScopePlatform},
	} {
		if r := g.Blocked(ctx, m); r != "" {
			t.Errorf("%s/%s must not be gated: %q", m.Channel, m.SenderScope, r)
		}
	}
	if calls.Load() != before {
		t.Fatal("free channels and platform sends must not call subscriptions-api")
	}
	// The cached answer expires, so a purchase takes effect within the ttl.
	now = now.Add(2 * time.Minute)
	g.Blocked(ctx, messaging.Message{TenantID: blocked.String(), Channel: "whatsapp"})
	if calls.Load() != before+1 {
		t.Fatalf("an expired answer must be asked again: %d calls", calls.Load())
	}
}

func TestSendGateLeavesUnknownAnswersToTheWorker(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	g := NewSendGate(c, func(_ context.Context, s string) (uuid.UUID, bool) { return uuid.New(), true }, time.Minute, zap.NewNop())
	if r := g.Blocked(context.Background(), messaging.Message{TenantID: "x", Channel: "sms"}); r != "" {
		t.Fatalf("an unreachable billing service must not block at the source: %q", r)
	}
	unresolved := NewSendGate(c, func(context.Context, string) (uuid.UUID, bool) { return uuid.Nil, false }, time.Minute, zap.NewNop())
	if r := unresolved.Blocked(context.Background(), messaging.Message{TenantID: "ghost", Channel: "sms"}); r != "" {
		t.Fatalf("an unresolved tenant must be left to the worker: %q", r)
	}
}

// Publish drops a gated message as handled: no error, so consumers neither retry nor log it.
func TestPublishDropsGatedMessagesWithoutAnError(t *testing.T) {
	messaging.SetChannelGate(func(_ context.Context, m messaging.Message) string {
		if m.Channel == "sms" {
			return "no SMS plan"
		}
		return ""
	})
	t.Cleanup(func() { messaging.SetChannelGate(nil) })
	if id, err := messaging.Publish(context.Background(), nil, config.EventsConfig{}, messaging.Message{Channel: "sms"}); err != nil || id != "" {
		t.Fatalf("gated message: id=%q err=%v", id, err)
	}
	// An allowed message reaches the bus (here: none configured, so it errors as before).
	if _, err := messaging.Publish(context.Background(), nil, config.EventsConfig{}, messaging.Message{Channel: "email"}); err == nil {
		t.Fatal("an allowed message must still go to the event bus")
	}
}
