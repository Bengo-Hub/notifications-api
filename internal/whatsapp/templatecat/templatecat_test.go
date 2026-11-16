package templatecat

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// memBackend is an in-memory backend for tests.
type memBackend struct {
	mu     sync.Mutex
	hashes map[string]map[string]string
	keys   map[string]string
}

func newMem() *memBackend {
	return &memBackend{hashes: map[string]map[string]string{}, keys: map[string]string{}}
}

func (m *memBackend) HGetAll(_ context.Context, key string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.hashes[key] {
		out[k] = v
	}
	return out, nil
}

func (m *memBackend) HSet(_ context.Context, key, field, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hashes[key] == nil {
		m.hashes[key] = map[string]string{}
	}
	m.hashes[key][field] = value
	return nil
}

func (m *memBackend) SetNX(_ context.Context, key, value string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[key]; ok {
		return false, nil
	}
	m.keys[key] = value
	return true, nil
}

func (m *memBackend) Get(_ context.Context, key string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.keys[key]
	return v, ok, nil
}

func (m *memBackend) Set(_ context.Context, key, value string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[key] = value
	return nil
}

func newStore(t *testing.T) (*Store, *memBackend) {
	t.Helper()
	b := newMem()
	return newStoreWith(b), b
}

// Set records Meta's category and reports a change only when one was already known and differs.
func TestSetReportsChanges(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if prev, changed, err := s.Set(ctx, "waba1", "t1", "utility"); err != nil || changed || prev != "" {
		t.Fatalf("first set: %q %v %v", prev, changed, err)
	}
	if _, changed, _ := s.Set(ctx, "waba1", "t1", "UTILITY"); changed {
		t.Fatal("same category reported as a change")
	}
	prev, changed, err := s.Set(ctx, "waba1", "t1", "MARKETING")
	if err != nil || !changed || prev != "UTILITY" {
		t.Fatalf("move: %q %v %v", prev, changed, err)
	}
	all, err := s.All(ctx)
	if err != nil || all["t1"] != "MARKETING" {
		t.Fatalf("all = %v %v", all, err)
	}
}

// A template name on two WABAs with different categories bills as the dearer one, so a send is
// never charged below what Meta may bill for it.
func TestNameCategoryIsTheDearest(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	_, _, _ = s.Set(ctx, "own", "t1", "UTILITY")
	_, _, _ = s.Set(ctx, "shared", "t1", "MARKETING")
	_, _, _ = s.Set(ctx, "own", "otp", "AUTHENTICATION")
	all, _ := s.All(ctx)
	if all["t1"] != "MARKETING" || all["otp"] != "AUTHENTICATION" {
		t.Fatalf("all = %v", all)
	}
	// Back to utility on the shared WABA: the name follows.
	_, _, _ = s.Set(ctx, "shared", "t1", "UTILITY")
	if all, _ = s.All(ctx); all["t1"] != "UTILITY" {
		t.Fatalf("after move back: %v", all)
	}
}

// The worker's cache answers from memory and picks up the store on Refresh.
func TestCacheLookup(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	c := NewCache(s)
	if _, ok := c.Lookup("t1"); ok {
		t.Fatal("empty cache answered")
	}
	_, _, _ = s.Set(ctx, "w", "t1", "MARKETING")
	if err := c.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if cat, ok := c.Lookup("t1"); !ok || cat != "MARKETING" {
		t.Fatalf("lookup = %q %v", cat, ok)
	}
	var nilCache *Cache
	if _, ok := nilCache.Lookup("t1"); ok {
		t.Fatal("nil cache answered")
	}
}

// Claim lets one replica act on an event; the same key a second time is refused.
func TestClaimOnce(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if !s.Claim(ctx, "k", 0) {
		t.Fatal("first claim refused")
	}
	if s.Claim(ctx, "k", 0) {
		t.Fatal("second claim allowed")
	}
}

type alerts []Change

func (a *alerts) AlertWhatsAppTemplateChange(_ context.Context, c Change) { *a = append(*a, c) }

// The first sweep fills the store silently; a later sweep alerts on what Meta moved since.
func TestSweepAlertsOnMoves(t *testing.T) {
	s, b := newStore(t)
	ctx := context.Background()
	cats := map[string]string{"a": "UTILITY", "b": "UTILITY"}
	fetch := func(context.Context, Account) (map[string]templatesync.ExistingTemplate, error) {
		out := map[string]templatesync.ExistingTemplate{}
		for n, c := range cats {
			out[n] = templatesync.ExistingTemplate{Status: "APPROVED", Category: c}
		}
		return out, nil
	}
	var got alerts
	accs := []Account{{WABAID: "W", Token: "T"}, {WABAID: "W", Token: "T2"}, {WABAID: "X"}}
	if n := s.Sweep(ctx, accs, fetch, &got, nil); n != 0 || len(got) != 0 {
		t.Fatalf("first sweep: %d moved, %v", n, got)
	}
	cats["b"] = "MARKETING"
	delete(b.keys, keyClaim+"sweep") // the next interval
	if n := s.Sweep(ctx, accs, fetch, &got, nil); n != 1 || len(got) != 1 || got[0].Name != "b" ||
		got[0].PreviousCategory != "UTILITY" || got[0].NewCategory != "MARKETING" || got[0].Source != "sweep" {
		t.Fatalf("second sweep: %d moved, %+v", n, got)
	}
	// Another replica in the same interval does nothing.
	if n := s.Sweep(ctx, accs, fetch, &got, nil); n != 0 {
		t.Fatalf("claimed interval swept again: %d", n)
	}
}

// The status of a send, as Meta's webhook reported it, for the Test button to read.
func TestSendStatus(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, ok := s.SendStatus(ctx, "wamid.1"); ok {
		t.Fatal("unknown send answered")
	}
	s.RecordSendStatus(ctx, SendStatus{MessageID: "wamid.1", Status: "sent"})
	s.RecordSendStatus(ctx, SendStatus{MessageID: "wamid.1", Status: "failed", Code: 131049, Title: "healthy ecosystem"})
	// A late "sent" never hides the failure.
	s.RecordSendStatus(ctx, SendStatus{MessageID: "wamid.1", Status: "sent"})
	got, ok := s.SendStatus(ctx, "wamid.1")
	if !ok || got.Status != "failed" || got.Code != 131049 {
		t.Fatalf("got %+v %v", got, ok)
	}
}
