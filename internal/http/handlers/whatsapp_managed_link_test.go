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

// linkGraph answers like Meta's Graph API for a managed link. Paths not in answers get {"success":true}.
type linkGraph struct {
	mu      sync.Mutex
	calls   []string
	auth    map[string]string
	bodies  map[string]string
	answers map[string]graphAnswer // "METHOD /path" -> answer
}

type graphAnswer struct {
	status int
	body   string
}

func (g *linkGraph) server(t *testing.T) *httptest.Server {
	t.Helper()
	g.auth = map[string]string{}
	g.bodies = map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		g.calls = append(g.calls, key)
		g.auth[r.URL.Path] = r.Header.Get("Authorization")
		g.bodies[r.URL.Path] = string(b)
		if a, ok := g.answers[key]; ok {
			if a.status != 0 {
				w.WriteHeader(a.status)
			}
			_, _ = w.Write([]byte(a.body))
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// numericLinkGraph is number 109 in account 209, not yet on Cloud API, with a payment method.
func numericLinkGraph() *linkGraph {
	return &linkGraph{answers: map[string]graphAnswer{
		"GET /v26.0/109":               {body: `{"display_phone_number":"+254 700 000000","verified_name":"Hadia","platform_type":"NOT_APPLICABLE","status":"PENDING","id":"109"}`},
		"GET /v26.0/209/phone_numbers": {body: `{"data":[{"id":"108"},{"id":"109"}]}`},
		"GET /v26.0/209":               {body: `{"primary_funding_id":"card-1","id":"209"}`},
	}}
}

var linkCreds = map[string]string{"access_token": "platform-token", "api_version": "v26.0", "waba_id": "2633992870377940", "phone_number_id": "300"}

type linkRecorder struct {
	saved   savedSignup
	removed []string
}

func newLinkHandler(t *testing.T, base string) (*WhatsAppEmbeddedSignupHandler, *linkRecorder) {
	t.Helper()
	rec := &linkRecorder{}
	h := &WhatsAppEmbeddedSignupHandler{
		log:        zap.NewNop(),
		http:       http.DefaultClient,
		graphBase:  base,
		creds:      func(context.Context) (map[string]string, error) { return linkCreds, nil },
		encrypt:    func(v string) (string, error) { return "enc(" + v + ")", nil },
		newPIN:     func() (string, error) { return "654321", nil },
		platformID: "platform-tenant",
	}
	h.save = func(_ context.Context, tenantID string, rows []signupSetting) error {
		rec.saved.tenant = tenantID
		rec.saved.rows = rows
		return nil
	}
	h.removeKeys = func(_ context.Context, tenantID string, keys ...string) error {
		if tenantID != "hadia" {
			t.Errorf("removed keys of tenant %q", tenantID)
		}
		rec.removed = append(rec.removed, keys...)
		return nil
	}
	h.linkedElsewhere = func(context.Context, string, string, string) (bool, error) { return false, nil }
	return h, rec
}

func postLink(h *WhatsAppEmbeddedSignupHandler, platformOwner bool, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/billing/whatsapp/managed-link", bytes.NewBufferString(body))
	r.Header.Set("X-Tenant-ID", "hadia")
	ctx := httpware.WithTenantID(r.Context(), "platform-tenant")
	if platformOwner {
		ctx = context.WithValue(ctx, httpware.IsPlatformOwnerKey, true)
	}
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h.ManagedLink(w, r)
	return w
}

const linkBody = `{"phone_number_id":"109","waba_id":"209"}`

// Only Meta's numeric IDs reach a Graph URL.
func TestManagedLinkNeedsNumericIDs(t *testing.T) {
	g := numericLinkGraph()
	srv := g.server(t)
	h, _ := newLinkHandler(t, srv.URL)
	for _, body := range []string{`{"phone_number_id":"109/register","waba_id":"209"}`, `{"phone_number_id":"109"}`, `not json`} {
		if w := postLink(h, true, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", body, w.Code)
		}
	}
	if len(g.calls) != 0 {
		t.Fatalf("calls = %v", g.calls)
	}
}

func TestManagedLinkHappyPath(t *testing.T) {
	g := numericLinkGraph()
	srv := g.server(t)
	h, rec := newLinkHandler(t, srv.URL)
	w := postLink(h, true, linkBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	want := []string{"GET /v26.0/109", "GET /v26.0/209/phone_numbers", "POST /v26.0/209/subscribed_apps", "POST /v26.0/109/register", "GET /v26.0/209"}
	if strings.Join(g.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", g.calls, want)
	}
	for p, a := range g.auth {
		if a != "Bearer platform-token" {
			t.Errorf("%s used %q, want the platform token", p, a)
		}
	}
	var reg map[string]string
	_ = json.Unmarshal([]byte(g.bodies["/v26.0/109/register"]), &reg)
	if reg["pin"] != "654321" || reg["messaging_product"] != "whatsapp" {
		t.Errorf("register body = %v", reg)
	}
	if rec.saved.tenant != "hadia" {
		t.Fatalf("saved for tenant %q, want the X-Tenant-ID tenant", rec.saved.tenant)
	}
	got := map[string]signupSetting{}
	for _, r := range rec.saved.rows {
		got[r.key] = r
	}
	if got["phone_number_id"].value != "109" || got["waba_id"].value != "209" {
		t.Errorf("rows = %+v", rec.saved.rows)
	}
	if ts := got["token_source"]; ts.value != "platform" || ts.secret {
		t.Errorf("token_source row = %+v, want plain \"platform\"", ts)
	}
	if pin := got["registration_pin"]; pin.value != "enc(654321)" || !pin.secret {
		t.Errorf("registration_pin row = %+v", pin)
	}
	if _, ok := got["access_token"]; ok {
		t.Error("a managed link must not store a tenant token")
	}
	if strings.Join(rec.removed, ",") != "access_token" {
		t.Errorf("removed %v, want the old tenant access_token", rec.removed)
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["verified_name"] != "Hadia" || body["payment_method_missing"] != nil {
		t.Errorf("response = %v", body)
	}
}

func TestManagedLinkPlatformOwnerOnly(t *testing.T) {
	g := numericLinkGraph()
	srv := g.server(t)
	h, rec := newLinkHandler(t, srv.URL)
	w := postLink(h, false, `{"phone_number_id":"109","waba_id":"209"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
	if len(g.calls) != 0 || rec.saved.tenant != "" {
		t.Fatalf("a refused caller reached Meta (%v) or saved rows", g.calls)
	}
}

func TestManagedLinkRefusesPlatformAccount(t *testing.T) {
	for _, body := range []string{
		`{"phone_number_id":"109","waba_id":"2633992870377940"}`,
		`{"phone_number_id":"300","waba_id":"209"}`,
	} {
		g := numericLinkGraph()
		srv := g.server(t)
		h, rec := newLinkHandler(t, srv.URL)
		w := postLink(h, true, body)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "platform's own") {
			t.Fatalf("%s: status %d: %s", body, w.Code, w.Body.String())
		}
		if len(g.calls) != 0 || rec.saved.tenant != "" {
			t.Fatalf("%s: reached Meta (%v) or saved", body, g.calls)
		}
	}
}

func TestManagedLinkRefusesPlatformTenantAsTarget(t *testing.T) {
	h, _ := newLinkHandler(t, "http://unused")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/billing/whatsapp/managed-link", bytes.NewBufferString(`{"phone_number_id":"109","waba_id":"209"}`))
	ctx := context.WithValue(httpware.WithTenantID(r.Context(), "platform-tenant"), httpware.IsPlatformOwnerKey, true)
	w := httptest.NewRecorder()
	h.ManagedLink(w, r.WithContext(ctx))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 when no tenant is named", w.Code)
	}
}

func TestManagedLinkNumberNotInAccount(t *testing.T) {
	g := numericLinkGraph()
	g.answers["GET /v26.0/209/phone_numbers"] = graphAnswer{body: `{"data":[{"id":"108"}]}`}
	srv := g.server(t)
	h, rec := newLinkHandler(t, srv.URL)
	w := postLink(h, true, `{"phone_number_id":"109","waba_id":"209"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not in WhatsApp account") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(strings.Join(g.calls, "|"), "POST") || rec.saved.tenant != "" {
		t.Fatalf("calls %v / saved %q after a mismatch", g.calls, rec.saved.tenant)
	}
}

func TestManagedLinkMetaRefusesSystemUser(t *testing.T) {
	g := numericLinkGraph()
	g.answers["GET /v26.0/109"] = graphAnswer{status: http.StatusBadRequest,
		body: `{"error":{"message":"Unsupported get request. Object with ID '109' does not exist, cannot be loaded due to missing permissions","code":100}}`}
	srv := g.server(t)
	h, rec := newLinkHandler(t, srv.URL)
	w := postLink(h, true, `{"phone_number_id":"109","waba_id":"209"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", w.Code)
	}
	msg := w.Body.String()
	if !strings.Contains(msg, "missing permissions") || !strings.Contains(msg, "system user") {
		t.Errorf("message should carry Meta's words and the system user hint: %s", msg)
	}
	if rec.saved.tenant != "" {
		t.Fatal("saved after Meta refused")
	}
}

func TestManagedLinkAlreadyLinkedElsewhere(t *testing.T) {
	g := numericLinkGraph()
	srv := g.server(t)
	h, rec := newLinkHandler(t, srv.URL)
	var asked [3]string
	h.linkedElsewhere = func(_ context.Context, tenantID, phoneID, wabaID string) (bool, error) {
		asked = [3]string{tenantID, phoneID, wabaID}
		return true, nil
	}
	w := postLink(h, true, `{"phone_number_id":"109","waba_id":"209"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "another tenant") {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if asked != [3]string{"hadia", "109", "209"} {
		t.Errorf("checked %v", asked)
	}
	if len(g.calls) != 0 || rec.saved.tenant != "" {
		t.Fatalf("reached Meta (%v) or saved", g.calls)
	}
}

func TestManagedLinkSkipsRegisterOnCloudAPI(t *testing.T) {
	g := numericLinkGraph()
	g.answers["GET /v26.0/109"] = graphAnswer{body: `{"display_phone_number":"+254 700 000000","verified_name":"Hadia","platform_type":"CLOUD_API","status":"CONNECTED","id":"109"}`}
	g.answers["GET /v26.0/209"] = graphAnswer{body: `{"id":"209"}`} // no card attached yet
	srv := g.server(t)
	h, rec := newLinkHandler(t, srv.URL)
	w := postLink(h, true, `{"phone_number_id":"109","waba_id":"209"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(strings.Join(g.calls, "|"), "/register") {
		t.Fatalf("calls = %v: a number already on Cloud API must not be registered again", g.calls)
	}
	for _, r := range rec.saved.rows {
		if r.key == "registration_pin" {
			t.Fatal("no PIN is saved when we did not set one")
		}
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["payment_method_missing"] != true {
		t.Errorf("the account has no primary_funding_id, want payment_method_missing: %v", body)
	}
}

// Embedded Signup after a managed link drops the marker, so the tenant's new business token wins.
func TestEmbeddedSignupRemovesManagedLinkMarker(t *testing.T) {
	g := &fakeGraph{}
	srv := g.server(t)
	h, _ := newSignupHandler(t, srv.URL, fullCreds)
	var removed []string
	h.removeKeys = func(_ context.Context, _ string, keys ...string) error {
		removed = append(removed, keys...)
		return nil
	}
	if w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Join(removed, ",") != "token_source" {
		t.Fatalf("removed %v, want token_source", removed)
	}
}

func TestDisconnectRemovesTokenSource(t *testing.T) {
	found := false
	for _, k := range connectionKeys {
		if k == "token_source" {
			found = true
		}
	}
	if !found {
		t.Fatalf("connectionKeys %v must include token_source so Disconnect removes it", connectionKeys)
	}
}

func TestManagedWhatsAppEditRefusal(t *testing.T) {
	managed := map[string]string{"phone_number_id": "109", "waba_id": "209", "token_source": "platform"}
	cases := []struct {
		name     string
		po       bool
		existing map[string]string
		settings map[string]string
		refused  bool
	}{
		{name: "tenant sets the marker", existing: map[string]string{}, settings: map[string]string{"token_source": "platform", "phone_number_id": "1"}, refused: true},
		{name: "tenant moves a managed number", existing: managed, settings: map[string]string{"phone_number_id": "555"}, refused: true},
		{name: "tenant moves a managed account", existing: managed, settings: map[string]string{"waba_id": "555"}, refused: true},
		{name: "tenant resaves unchanged", existing: managed, settings: map[string]string{"phone_number_id": "109", "waba_id": "209", "token_source": "platform"}},
		{name: "tenant edits other fields", existing: managed, settings: map[string]string{"api_version": "v26.0", "phone_number_id": ""}},
		{name: "tenant edits its own number", existing: map[string]string{"phone_number_id": "1"}, settings: map[string]string{"phone_number_id": "2"}},
		{name: "platform owner may change it", po: true, existing: managed, settings: map[string]string{"phone_number_id": "555"}},
	}
	for _, c := range cases {
		got := managedWhatsAppEditRefusal("whatsapp", "meta_cloud", c.po, c.existing, c.settings) != ""
		if got != c.refused {
			t.Errorf("%s: refused = %v, want %v", c.name, got, c.refused)
		}
	}
	if managedWhatsAppEditRefusal("email", "smtp", false, managed, map[string]string{"token_source": "x"}) != "" {
		t.Error("other providers are not affected")
	}
}
