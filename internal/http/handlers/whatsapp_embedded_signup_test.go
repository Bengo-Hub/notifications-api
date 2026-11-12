package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	httpware "github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"
)

// fakeGraph records the calls Embedded Signup makes and answers like Meta's Graph API.
type fakeGraph struct {
	mu       sync.Mutex
	calls    []string
	auth     map[string]string // path -> Authorization header
	bodies   map[string]string
	exchange string // query string of the code exchange
	failPath string
}

func (g *fakeGraph) server(t *testing.T) *httptest.Server {
	t.Helper()
	g.auth = map[string]string{}
	g.bodies = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		g.calls = append(g.calls, r.Method+" "+r.URL.Path)
		g.auth[r.URL.Path] = r.Header.Get("Authorization")
		g.bodies[r.URL.Path] = string(b)
		if r.URL.Path == g.failPath {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"nope","code":100}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/oauth/access_token") {
			g.exchange = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"access_token":"business-token","token_type":"bearer"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type savedSignup struct {
	tenant string
	rows   []signupSetting
}

func newSignupHandler(t *testing.T, base string, creds map[string]string) (*WhatsAppEmbeddedSignupHandler, *savedSignup) {
	t.Helper()
	saved := &savedSignup{}
	h := &WhatsAppEmbeddedSignupHandler{
		log:       zap.NewNop(),
		http:      http.DefaultClient,
		graphBase: base,
		creds:     func(context.Context) (map[string]string, error) { return creds, nil },
		encrypt:   func(v string) (string, error) { return "enc(" + v + ")", nil },
		newPIN:    func() (string, error) { return "123456", nil },
	}
	h.save = func(_ context.Context, tenantID string, rows []signupSetting) error {
		saved.tenant = tenantID
		saved.rows = rows
		return nil
	}
	return h, saved
}

func postSignup(h *WhatsAppEmbeddedSignupHandler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/billing/whatsapp/embedded-signup/complete", bytes.NewBufferString(body))
	r = r.WithContext(httpware.WithTenantID(r.Context(), "tenant-1"))
	w := httptest.NewRecorder()
	h.Complete(w, r)
	return w
}

var fullCreds = map[string]string{"app_id": "app-1", "app_secret": "shh", "access_token": "platform-token", "api_version": "v21.0"}

// The fix for "Object with ID ... does not exist, cannot be loaded due to missing permissions":
// the platform token has no rights on a WABA a customer just created, so the signup code must be
// exchanged for the business token and that token used for every call on the new WABA.
func TestEmbeddedSignupExchangesCodeAndUsesBusinessToken(t *testing.T) {
	g := &fakeGraph{}
	srv := g.server(t)
	h, saved := newSignupHandler(t, srv.URL, fullCreds)

	w := postSignup(h, `{"code":"the-code","waba_id":"W1","phone_number_id":"P1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	want := []string{"GET /v21.0/oauth/access_token", "POST /v21.0/W1/subscribed_apps", "POST /v21.0/P1/register"}
	if strings.Join(g.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", g.calls, want)
	}
	for _, q := range []string{"client_id=app-1", "client_secret=shh", "code=the-code"} {
		if !strings.Contains(g.exchange, q) {
			t.Errorf("code exchange query %q missing %q", g.exchange, q)
		}
	}
	for _, p := range []string{"/v21.0/W1/subscribed_apps", "/v21.0/P1/register"} {
		if g.auth[p] != "Bearer business-token" {
			t.Errorf("%s used %q, want the business token", p, g.auth[p])
		}
	}
	var reg map[string]string
	_ = json.Unmarshal([]byte(g.bodies["/v21.0/P1/register"]), &reg)
	if reg["messaging_product"] != "whatsapp" || reg["pin"] != "123456" {
		t.Errorf("register body = %v", reg)
	}
	if saved.tenant != "tenant-1" {
		t.Fatalf("saved for tenant %q", saved.tenant)
	}
	got := map[string]signupSetting{}
	for _, r := range saved.rows {
		got[r.key] = r
	}
	if got["phone_number_id"].value != "P1" || got["phone_number_id"].secret {
		t.Errorf("phone_number_id row = %+v", got["phone_number_id"])
	}
	if got["waba_id"].value != "W1" {
		t.Errorf("waba_id row = %+v", got["waba_id"])
	}
	if r := got["access_token"]; r.value != "enc(business-token)" || !r.secret {
		t.Errorf("access_token must be stored encrypted, got %+v", r)
	}
	if r := got["registration_pin"]; r.value != "enc(123456)" || !r.secret {
		t.Errorf("registration_pin must be stored encrypted, got %+v", r)
	}
	if strings.Contains(w.Body.String(), "business-token") || strings.Contains(w.Body.String(), "123456") {
		t.Error("response leaks the token or PIN")
	}
}

func TestEmbeddedSignupRequiresCode(t *testing.T) {
	h, _ := newSignupHandler(t, "http://unused", fullCreds)
	w := postSignup(h, `{"waba_id":"W1","phone_number_id":"P1"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

func TestEmbeddedSignupNeedsAppIDAndSecret(t *testing.T) {
	t.Setenv("WHATSAPP_APP_ID", "")
	t.Setenv("WHATSAPP_APP_SECRET", "")
	h, _ := newSignupHandler(t, "http://unused", map[string]string{"access_token": "platform-token"})
	w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", w.Code)
	}
	if !strings.Contains(w.Body.String(), "App ID") {
		t.Errorf("error should say what to set: %s", w.Body.String())
	}
}

func TestEmbeddedSignupStopsWhenExchangeFails(t *testing.T) {
	g := &fakeGraph{failPath: "/v21.0/oauth/access_token"}
	srv := g.server(t)
	h, saved := newSignupHandler(t, srv.URL, fullCreds)
	w := postSignup(h, `{"code":"bad","waba_id":"W1","phone_number_id":"P1"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", w.Code)
	}
	if len(g.calls) != 1 || saved.tenant != "" {
		t.Fatalf("must stop after a failed exchange: calls=%v saved=%q", g.calls, saved.tenant)
	}
	if strings.Contains(w.Body.String(), "shh") {
		t.Error("response leaks the app secret")
	}
}
