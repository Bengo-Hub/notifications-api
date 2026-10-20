package maildelivery

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
)

func sign(body, key string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func TestParseEvents(t *testing.T) {
	evs, err := ParseEvents([]byte(`{"events":[{"id":"1","createdAt":"2026-10-20T08:00:00Z","type":"delivery.failed","data":{"to":"a@x.com"}}]}`))
	if err != nil || len(evs) != 1 || evs[0].Type != EventDeliveryFailed {
		t.Fatalf("wrapped batch: %v %v", evs, err)
	}
	if evs, err = ParseEvents([]byte(`[{"id":"2","type":"delivery.failed","data":{}}]`)); err != nil || len(evs) != 1 {
		t.Fatalf("bare array: %v %v", evs, err)
	}
	if _, err = ParseEvents([]byte(`not json`)); err == nil {
		t.Fatal("garbage must be rejected")
	}
	if evs, err = ParseEvents([]byte(`{"events":[]}`)); err != nil || len(evs) != 0 {
		t.Fatalf("empty batch: %v %v", evs, err)
	}
}

func TestBounceFrom(t *testing.T) {
	cases := []struct {
		name       string
		event      Event
		wantOK     bool
		wantToken  string
		wantBad    bool
		wantRecipt string
	}{
		{"unknown user", Event{Type: EventDeliveryFailed, Data: map[string]any{
			"to": "Gone@Example.COM", "code": float64(550), "details": "5.1.1 The email account that you tried to reach does not exist",
			"messageId": "<7f3c1e2a-9b1d-4c1a-8d52-0a1b2c3d4e5f@codevertexafrica.com>"}},
			true, "7f3c1e2a-9b1d-4c1a-8d52-0a1b2c3d4e5f", true, "gone@example.com"},
		{"recipient list", Event{Type: EventDeliveryFailed, Data: map[string]any{
			"to": []any{"a@x.com", "b@x.com"}, "code": "554", "details": "5.7.1 blocked as spam"}},
			true, "", false, "a@x.com"},
		{"enhanced code only", Event{Type: EventDeliveryFailed, Data: map[string]any{
			"to": "a@x.com", "reason": "mailbox unavailable 5.1.2"}},
			true, "", true, "a@x.com"},
		{"temporary failure", Event{Type: EventDeliveryFailed, Data: map[string]any{
			"to": "a@x.com", "code": float64(452), "details": "4.2.2 mailbox full"}}, false, "", false, ""},
		{"other event", Event{Type: "delivery.delivered", Data: map[string]any{"to": "a@x.com", "code": float64(250)}}, false, "", false, ""},
		{"no recipient", Event{Type: EventDeliveryFailed, Data: map[string]any{"code": float64(550)}}, false, "", false, ""},
		{"generated message id carries no token", Event{Type: EventDeliveryFailed, Data: map[string]any{
			"to": "a@x.com", "code": float64(550), "messageId": "<1760947200000000000.ab12cd34@codevertexafrica.com>"}},
			true, "", false, "a@x.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, ok := BounceFrom(c.event)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if b.Token != c.wantToken || b.BadAddress != c.wantBad || b.Recipient != c.wantRecipt {
				t.Fatalf("got %+v", b)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	body := `{"events":[]}`
	good := sign(body, "k1")
	if !Verify([]byte(body), good, "k1") {
		t.Fatal("valid signature rejected")
	}
	if !Verify([]byte(body), good, "other", "k1") {
		t.Fatal("previous key must still verify")
	}
	if Verify([]byte(body), good, "k2") {
		t.Fatal("wrong key accepted")
	}
	if Verify([]byte(body+" "), good, "k1") {
		t.Fatal("changed body accepted")
	}
	if Verify([]byte(body), "", "k1") || Verify([]byte(body), good) || Verify([]byte(body), good, "") {
		t.Fatal("empty signature or key must never verify")
	}
}

type fakeStore struct {
	calls [][2]string
	n     int
	err   error
}

func (f *fakeStore) MarkBounced(_ context.Context, token, recipient string) (int, error) {
	f.calls = append(f.calls, [2]string{token, recipient})
	return f.n, f.err
}

func TestServiceHandle(t *testing.T) {
	store := &fakeStore{n: 1}
	var suppressed []string
	svc := NewService(store, func(_ context.Context, addr string, _ time.Duration) { suppressed = append(suppressed, addr) }, zap.NewNop())
	res := svc.Handle(context.Background(), []Event{
		{Type: EventDeliveryFailed, Data: map[string]any{"to": "gone@x.com", "code": float64(550), "details": "5.1.1 user unknown"}},
		{Type: EventDeliveryFailed, Data: map[string]any{"to": "spam@x.com", "code": float64(554), "details": "5.7.1 policy"}},
		{Type: "delivery.delivered", Data: map[string]any{"to": "ok@x.com"}},
	})
	if res.Events != 3 || res.Bounced != 2 || res.Suppressed != 1 {
		t.Fatalf("result %+v", res)
	}
	if len(suppressed) != 1 || suppressed[0] != "gone@x.com" {
		t.Fatalf("only the missing mailbox is suppressed, got %v", suppressed)
	}
	if len(store.calls) != 2 {
		t.Fatalf("delivered events must not touch the store, calls %v", store.calls)
	}

	failing := NewService(&fakeStore{err: errors.New("db down")}, nil, zap.NewNop())
	res = failing.Handle(context.Background(), []Event{{Type: EventDeliveryFailed, Data: map[string]any{"to": "a@x.com", "code": float64(550)}}})
	if res.Bounced != 0 {
		t.Fatalf("a failed store write must not count as bounced: %+v", res)
	}
}

type memConfig struct {
	vals map[string]string
}

func (m *memConfig) Get(_ context.Context, key string) (string, time.Time, bool, error) {
	v, ok := m.vals[key]
	return v, time.Unix(1, 0), ok, nil
}

func (m *memConfig) Put(_ context.Context, key, value, _ string) error {
	if value == "" {
		delete(m.vals, key)
		return nil
	}
	m.vals[key] = value
	return nil
}

func TestRotate(t *testing.T) {
	cfg := &memConfig{vals: map[string]string{}}
	keys := NewKeys(cfg)
	ctx := context.Background()

	if ok, _ := keys.Configured(ctx); ok {
		t.Fatal("nothing is configured yet")
	}
	var applied string
	if _, err := keys.Rotate(ctx, func(_ context.Context, k string) error { applied = k; return nil }); err != nil {
		t.Fatal(err)
	}
	first := cfg.vals[KeyConfigKey]
	if first == "" || first != applied || len(first) < 40 {
		t.Fatalf("first rotation: stored %q applied %q", first, applied)
	}
	if _, has := cfg.vals[PreviousKeyConfigKey]; has {
		t.Fatal("no previous key after the first rotation")
	}

	if _, err := keys.Rotate(ctx, func(_ context.Context, k string) error { applied = k; return nil }); err != nil {
		t.Fatal(err)
	}
	if cfg.vals[PreviousKeyConfigKey] != first || cfg.vals[KeyConfigKey] == first || cfg.vals[KeyConfigKey] != applied {
		t.Fatalf("second rotation must keep the first key as previous: %v", cfg.vals)
	}
	active, err := keys.Active(ctx)
	if err != nil || len(active) != 2 || active[0] != cfg.vals[KeyConfigKey] || active[1] != first {
		t.Fatalf("active keys %v %v", active, err)
	}

	// A mail server that refuses the key leaves the stored keys exactly as they were.
	before := map[string]string{KeyConfigKey: cfg.vals[KeyConfigKey], PreviousKeyConfigKey: cfg.vals[PreviousKeyConfigKey]}
	if _, err := keys.Rotate(ctx, func(context.Context, string) error { return errors.New("boom") }); err == nil {
		t.Fatal("failed apply must fail the rotation")
	}
	if cfg.vals[KeyConfigKey] != before[KeyConfigKey] || cfg.vals[PreviousKeyConfigKey] != before[PreviousKeyConfigKey] {
		t.Fatalf("keys changed after a failed rotation: %v", cfg.vals)
	}
	active, _ = keys.Active(ctx)
	if len(active) != 2 || active[0] != before[KeyConfigKey] {
		t.Fatalf("cache must not serve the failed key: %v", active)
	}
	if _, err := keys.Rotate(ctx, nil); err == nil {
		t.Fatal("a nil applier must be refused")
	}
}

func TestProvisionerApplier(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	apply := ProvisionerApplier(srv.URL+"/", "http://notifications-api/hook", nil)
	if err := apply(context.Background(), "secret-key"); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPut || gotPath != "/internal/delivery-webhook" ||
		gotBody != `{"key":"secret-key","url":"http://notifications-api/hook"}` {
		t.Fatalf("request %s %s %s", gotMethod, gotPath, gotBody)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "stalwart said no", http.StatusBadGateway)
	}))
	defer bad.Close()
	if err := ProvisionerApplier(bad.URL, "u", nil)(context.Background(), "k"); err == nil {
		t.Fatal("a non-2xx answer must be an error")
	}
}
