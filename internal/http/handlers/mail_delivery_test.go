package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/maildelivery"
)

type memKeyConfig struct{ vals map[string]string }

func (m *memKeyConfig) Get(_ context.Context, key string) (string, time.Time, bool, error) {
	v, ok := m.vals[key]
	return v, time.Unix(1, 0), ok, nil
}

func (m *memKeyConfig) Put(_ context.Context, key, value, _ string) error {
	if value == "" {
		delete(m.vals, key)
		return nil
	}
	m.vals[key] = value
	return nil
}

type recordingStore struct{ calls int }

func (r *recordingStore) MarkBounced(context.Context, string, string) (int, error) {
	r.calls++
	return 1, nil
}

func signBody(body, key string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func newMailDeliveryTestHandler(cfg *memKeyConfig, store *recordingStore) *MailDeliveryHandler {
	log := zap.NewNop()
	return NewMailDeliveryHandler(maildelivery.NewService(store, nil, log), maildelivery.NewKeys(cfg), nil, nil, "http://hook", log)
}

func TestMailDeliveryWebhookAuthentication(t *testing.T) {
	const body = `{"events":[{"id":"1","type":"delivery.failed","data":{"to":"gone@x.com","code":550,"details":"5.1.1 unknown"}}]}`
	post := func(h *MailDeliveryHandler, signature string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/stalwart/delivery", strings.NewReader(body))
		if signature != "" {
			req.Header.Set("X-Signature", signature)
		}
		rec := httptest.NewRecorder()
		h.Webhook(rec, req)
		return rec.Code
	}

	// No key configured yet: the endpoint refuses everything rather than accepting unsigned calls.
	store := &recordingStore{}
	h := newMailDeliveryTestHandler(&memKeyConfig{vals: map[string]string{}}, store)
	if got := post(h, signBody(body, "anything")); got != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured webhook answered %d, want 503", got)
	}

	cfg := &memKeyConfig{vals: map[string]string{maildelivery.KeyConfigKey: "current", maildelivery.PreviousKeyConfigKey: "older"}}
	store = &recordingStore{}
	h = newMailDeliveryTestHandler(cfg, store)

	if got := post(h, ""); got != http.StatusUnauthorized {
		t.Fatalf("unsigned call answered %d, want 401", got)
	}
	if got := post(h, signBody(body, "wrong")); got != http.StatusUnauthorized {
		t.Fatalf("wrongly signed call answered %d, want 401", got)
	}
	if store.calls != 0 {
		t.Fatal("nothing may be recorded for a call that failed authentication")
	}
	if got := post(h, signBody(body, "current")); got != http.StatusNoContent {
		t.Fatalf("correctly signed call answered %d, want 204", got)
	}
	if got := post(h, signBody(body, "older")); got != http.StatusNoContent {
		t.Fatalf("a call signed with the previous key answered %d, want 204", got)
	}
	if store.calls != 2 {
		t.Fatalf("recorded %d bounces, want 2", store.calls)
	}
}

func TestMailDeliveryStatusNeverShowsTheKey(t *testing.T) {
	cfg := &memKeyConfig{vals: map[string]string{maildelivery.KeyConfigKey: "super-secret-signing-key"}}
	h := newMailDeliveryTestHandler(cfg, &recordingStore{})
	rec := httptest.NewRecorder()
	h.Status(rec, httptest.NewRequest(http.MethodGet, "/api/v1/platform/mail-delivery-key", nil))
	out := rec.Body.String()
	if strings.Contains(out, "super-secret-signing-key") {
		t.Fatalf("status leaked the key: %s", out)
	}
	if !strings.Contains(out, `"configured":true`) || !strings.Contains(out, `"key_fingerprint":"`) {
		t.Fatalf("status %s", out)
	}
}

func TestMailDeliveryRotateRequiresGenerate(t *testing.T) {
	h := newMailDeliveryTestHandler(&memKeyConfig{vals: map[string]string{}}, &recordingStore{})
	rec := httptest.NewRecorder()
	h.Rotate(rec, httptest.NewRequest(http.MethodPut, "/api/v1/platform/mail-delivery-key", strings.NewReader(`{"key":"mine"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a hand-supplied key answered %d, want 400", rec.Code)
	}
}
