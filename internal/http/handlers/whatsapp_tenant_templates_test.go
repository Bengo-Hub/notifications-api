package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	httpware "github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/whatsapp/tenanttemplates"
)

// fakeTemplateGraph answers Meta's message_templates endpoints: auth_otp is already approved and
// ordering_order_placed_v3_btn was rejected; every create succeeds.
type fakeTemplateGraph struct {
	mu    sync.Mutex
	calls []string
	auth  []string
}

func (g *fakeTemplateGraph) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.calls = append(g.calls, r.Method+" "+r.URL.Path)
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"name":"auth_otp","status":"APPROVED","rejected_reason":"NONE"},` +
				`{"name":"ordering_order_placed_v3_btn","status":"REJECTED","rejected_reason":"INVALID_FORMAT"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1","status":"PENDING"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *fakeTemplateGraph) posts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, http.MethodPost) {
			n++
		}
	}
	return n
}

// newTenantTemplatesHandler wires the real tenant template service to a fake Graph server. The
// tenant uses ordering; creds records which tenant was resolved.
func newTenantTemplatesHandler(t *testing.T, graph string, credsErr error) (*WhatsAppTemplates, *[]string) {
	t.Helper()
	var mu sync.Mutex
	asked := &[]string{}
	creds := func(_ context.Context, tenantID string) (string, string, error) {
		mu.Lock()
		*asked = append(*asked, tenantID)
		mu.Unlock()
		if credsErr != nil {
			return "", "", credsErr
		}
		return "WABA-" + tenantID, "token-" + tenantID, nil
	}
	usage := func(context.Context, string) ([]string, error) { return []string{"ordering"}, nil }
	svc := tenanttemplates.New(creds, tenanttemplates.DefaultSelection(), zap.NewNop(), usage)
	svc.GraphURL = graph
	return NewWhatsAppTemplates(nil, zap.NewNop(), svc), asked
}

// serveAs runs handler behind the tenant middleware with a token for tokenTenant.
func serveAs(handler http.HandlerFunc, method, path, body, tokenTenant string, platformOwner bool, headerTenant string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if headerTenant != "" {
		r.Header.Set("X-Tenant-ID", headerTenant)
	}
	mw := httpware.TenantV2(httpware.TenantConfig{
		ClaimsExtractor: func(context.Context) (string, string, bool, bool) {
			return tokenTenant, "", platformOwner, true
		},
		URLParamFunc: func(*http.Request, string) string { return "" },
	})
	w := httptest.NewRecorder()
	mw(handler).ServeHTTP(w, r)
	return w
}

func TestTenantTemplateStatusHandler(t *testing.T) {
	g := &fakeTemplateGraph{}
	srv := g.server(t)
	h, asked := newTenantTemplatesHandler(t, srv.URL, nil)

	w := serveAs(h.TenantStatus, http.MethodGet, "/api/v1/settings/whatsapp/templates", "", "t1", false, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var rep tenanttemplates.StatusReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.WABAID != "WABA-t1" || len(*asked) != 1 || (*asked)[0] != "t1" {
		t.Fatalf("waba %q, creds asked for %v", rep.WABAID, *asked)
	}
	got := map[string]tenanttemplates.TemplateStatus{}
	for _, ts := range rep.Templates {
		got[ts.Name] = ts
	}
	if got["auth_otp"].Status != "APPROVED" || got["auth_otp"].Reason != "" {
		t.Errorf("auth_otp = %+v", got["auth_otp"])
	}
	if r := got["ordering_order_placed_v3_btn"]; r.Status != "REJECTED" || r.Reason != "INVALID_FORMAT" {
		t.Errorf("ordering_order_placed_v3_btn = %+v", r)
	}
	if got["subscription_invoice_ready_v1_btn"].Status != tenanttemplates.StatusMissing {
		t.Errorf("subscription_invoice_ready_v1_btn = %+v", got["subscription_invoice_ready_v1_btn"])
	}
	for name := range got {
		if strings.HasPrefix(name, "pos_") || strings.HasPrefix(name, "hadia_") || strings.HasPrefix(name, "codevertex_") {
			t.Errorf("%s listed for an ordering tenant", name)
		}
	}
	if g.posts() != 0 {
		t.Fatal("status must not write to Meta")
	}
	for i, c := range g.calls {
		if !strings.Contains(c, "/WABA-t1/") || g.auth[i] != "Bearer token-t1" {
			t.Fatalf("call %s with %q, want the tenant's own WABA and token", c, g.auth[i])
		}
	}
}

func TestTenantTemplateSyncHandler(t *testing.T) {
	g := &fakeTemplateGraph{}
	srv := g.server(t)
	h, _ := newTenantTemplatesHandler(t, srv.URL, nil)

	w := serveAs(h.TenantSync, http.MethodPost, "/api/v1/settings/whatsapp/templates/sync", `{"dry_run":true}`, "t1", false, "")
	if w.Code != http.StatusOK {
		t.Fatalf("dry run status %d: %s", w.Code, w.Body.String())
	}
	var dry tenanttemplates.SyncReport
	_ = json.Unmarshal(w.Body.Bytes(), &dry)
	if dry.Summary["would_create"] == 0 || dry.Summary["skipped"] != 2 || g.posts() != 0 {
		t.Fatalf("dry run summary %v, posts %d", dry.Summary, g.posts())
	}

	w = serveAs(h.TenantSync, http.MethodPost, "/api/v1/settings/whatsapp/templates/sync", ``, "t1", false, "")
	if w.Code != http.StatusOK {
		t.Fatalf("sync status %d: %s", w.Code, w.Body.String())
	}
	var live tenanttemplates.SyncReport
	_ = json.Unmarshal(w.Body.Bytes(), &live)
	if live.Summary["created"] == 0 || live.Summary["created"] > tenanttemplates.BatchSize {
		t.Fatalf("created %d, want 1..%d per call", live.Summary["created"], tenanttemplates.BatchSize)
	}
	if g.posts() != live.Summary["created"] {
		t.Fatalf("%d creates sent, summary says %d", g.posts(), live.Summary["created"])
	}
	if live.Remaining != dry.Summary["would_create"]-live.Summary["created"] {
		t.Fatalf("remaining %d, want %d", live.Remaining, dry.Summary["would_create"]-live.Summary["created"])
	}
}

// A tenant user can never reach another tenant's WABA through X-Tenant-ID; a platform owner may.
func TestTenantTemplateSyncTenantIsolation(t *testing.T) {
	g := &fakeTemplateGraph{}
	srv := g.server(t)

	h, asked := newTenantTemplatesHandler(t, srv.URL, nil)
	// Through the tenant middleware a mismatching header is refused outright.
	w := serveAs(h.TenantSync, http.MethodPost, "/api/v1/settings/whatsapp/templates/sync", `{"dry_run":true}`, "t1", false, "t2")
	if w.Code == http.StatusOK {
		t.Fatalf("a tenant user naming another tenant got 200: %s", w.Body.String())
	}
	// And the handler itself resolves the token's tenant even if a header slips through.
	for _, call := range []struct {
		fn     http.HandlerFunc
		method string
	}{{h.TenantSync, http.MethodPost}, {h.TenantStatus, http.MethodGet}} {
		r := httptest.NewRequest(call.method, "/api/v1/settings/whatsapp/templates/sync", bytes.NewBufferString(`{"dry_run":true}`))
		r.Header.Set("X-Tenant-ID", "t2")
		r = r.WithContext(httpware.WithTenantID(r.Context(), "t1"))
		rec := httptest.NewRecorder()
		call.fn(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status %d: %s", call.method, rec.Code, rec.Body.String())
		}
	}
	if len(*asked) != 2 {
		t.Fatalf("creds asked %v, want t1 twice", *asked)
	}
	for _, id := range *asked {
		if id != "t1" {
			t.Fatalf("a tenant user reached tenant %q through X-Tenant-ID", id)
		}
	}
	for _, c := range g.calls {
		if strings.Contains(c, "WABA-t2") {
			t.Fatalf("called %s for another tenant", c)
		}
	}

	h, asked = newTenantTemplatesHandler(t, srv.URL, nil)
	w = serveAs(h.TenantSync, http.MethodPost, "/api/v1/settings/whatsapp/templates/sync", `{"dry_run":true}`, "platform", true, "t2")
	if w.Code != http.StatusOK || len(*asked) != 1 || (*asked)[0] != "t2" {
		t.Fatalf("platform owner acting for t2: status %d, creds asked for %v", w.Code, *asked)
	}
}

func TestTenantTemplatesWithoutConnectedNumber(t *testing.T) {
	h, _ := newTenantTemplatesHandler(t, "http://unused", errors.New("no WhatsApp Business Account is connected for this tenant"))
	for _, call := range []struct {
		fn     http.HandlerFunc
		method string
	}{{h.TenantStatus, http.MethodGet}, {h.TenantSync, http.MethodPost}} {
		w := serveAs(call.fn, call.method, "/api/v1/settings/whatsapp/templates", "", "t1", false, "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "connected") {
			t.Fatalf("%s: status %d body %s, want 400 saying what to connect", call.method, w.Code, w.Body.String())
		}
	}
}

func TestTenantTemplatesNeedTenant(t *testing.T) {
	h, _ := newTenantTemplatesHandler(t, "http://unused", nil)
	w := serveAs(h.TenantSync, http.MethodPost, "/api/v1/settings/whatsapp/templates/sync", "", "", false, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}

// Owner decision "Auto-sync on connect": a completed signup starts the tenant's template sync.
func TestEmbeddedSignupStartsTemplateAutoSync(t *testing.T) {
	g := &fakeGraph{}
	srv := g.server(t)
	h, _ := newSignupHandler(t, srv.URL, fullCreds)
	started := make(chan string, 1)
	h.WithTemplateAutoSync(func(ctx context.Context, tenantID string) {
		if ctx.Err() != nil {
			t.Errorf("auto-sync context already done: %v", ctx.Err())
		}
		started <- tenantID
	})

	w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	select {
	case id := <-started:
		if id != "tenant-1" {
			t.Fatalf("auto-sync for %q, want tenant-1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-sync was not started after a successful signup")
	}
}

func TestEmbeddedSignupNoAutoSyncOnFailure(t *testing.T) {
	for _, fail := range []string{"/v21.0/oauth/access_token", "/v21.0/W1/subscribed_apps", "/v21.0/P1/register"} {
		g := &fakeGraph{failPath: fail}
		srv := g.server(t)
		h, _ := newSignupHandler(t, srv.URL, fullCreds)
		started := make(chan string, 1)
		h.WithTemplateAutoSync(func(_ context.Context, tenantID string) { started <- tenantID })

		w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`)
		if w.Code == http.StatusOK {
			t.Fatalf("%s failing still answered 200", fail)
		}
		select {
		case id := <-started:
			t.Fatalf("auto-sync started for %q although %s failed", id, fail)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestEmbeddedSignupNoAutoSyncWhenSaveFails(t *testing.T) {
	g := &fakeGraph{}
	srv := g.server(t)
	h, _ := newSignupHandler(t, srv.URL, fullCreds)
	h.save = func(context.Context, string, []signupSetting) error { return errors.New("db down") }
	started := make(chan string, 1)
	h.WithTemplateAutoSync(func(_ context.Context, tenantID string) { started <- tenantID })

	if w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	select {
	case <-started:
		t.Fatal("auto-sync started although the number was not saved")
	case <-time.After(100 * time.Millisecond):
	}
}
