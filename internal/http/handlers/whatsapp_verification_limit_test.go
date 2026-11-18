package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Two pods share one Redis count: the limit holds across handlers, the gap is enforced, and the
// window frees up again.
func TestVerifyLimitSharedAcrossPods(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	podA := (&WhatsAppEmbeddedSignupHandler{log: zap.NewNop(), now: clock}).WithVerifyLimitStore(rdb)
	podB := (&WhatsAppEmbeddedSignupHandler{log: zap.NewNop(), now: clock}).WithVerifyLimitStore(rdb)
	ctx := context.Background()

	// Gap: a second request on the other pod within 60s is refused with the remaining wait.
	if ok, _ := podA.allowAttempt(ctx, "request:t1", 5, time.Hour, time.Minute); !ok {
		t.Fatal("first request refused")
	}
	now = now.Add(20 * time.Second)
	ok, wait := podB.allowAttempt(ctx, "request:t1", 5, time.Hour, time.Minute)
	if ok || wait != 40*time.Second {
		t.Fatalf("second request within the gap: ok=%v wait=%v, want refused with 40s", ok, wait)
	}

	// Count: 3 checks allowed in total across both pods, the 4th refused until the window moves.
	for i, h := range []*WhatsAppEmbeddedSignupHandler{podA, podB, podA} {
		if ok, _ := h.allowAttempt(ctx, "verify:t1", 3, 15*time.Minute, 0); !ok {
			t.Fatalf("check %d refused", i+1)
		}
		now = now.Add(time.Second)
	}
	if ok, _ := podB.allowAttempt(ctx, "verify:t1", 3, 15*time.Minute, 0); ok {
		t.Fatal("4th check allowed, want refused")
	}
	now = now.Add(15 * time.Minute)
	if ok, _ := podB.allowAttempt(ctx, "verify:t1", 3, 15*time.Minute, 0); !ok {
		t.Fatal("check after the window refused")
	}

	// Another tenant has its own count.
	if ok, _ := podA.allowAttempt(ctx, "verify:t2", 3, 15*time.Minute, 0); !ok {
		t.Fatal("other tenant refused")
	}
}

// Without Redis the limit still applies inside the process.
func TestVerifyLimitInProcessFallback(t *testing.T) {
	h := &WhatsAppEmbeddedSignupHandler{log: zap.NewNop()}
	ctx := context.Background()
	if ok, _ := h.allowAttempt(ctx, "verify:t1", 1, time.Minute, 0); !ok {
		t.Fatal("first refused")
	}
	if ok, _ := h.allowAttempt(ctx, "verify:t1", 1, time.Minute, 0); ok {
		t.Fatal("second allowed")
	}
}
