package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	httpware "github.com/Bengo-Hub/httpware"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/whatsapp/profile"
)

// fakeProfileGraph answers the business profile and upload endpoints like Meta.
type fakeProfileGraph struct {
	mu    sync.Mutex
	calls []string
	auth  []string
	fail  string
}

func (g *fakeProfileGraph) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		g.mu.Lock()
		g.calls = append(g.calls, r.Method+" "+r.URL.Path)
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		g.mu.Unlock()
		if g.fail != "" && strings.HasSuffix(r.URL.Path, g.fail) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"(#131009) Parameter value is not valid","code":131009}}`))
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			_, _ = w.Write([]byte(`{"id":"upload:S1"}`))
		case strings.Contains(r.URL.Path, "/upload:"):
			_, _ = w.Write([]byte(`{"h":"H1"}`))
		case strings.HasSuffix(r.URL.Path, "/whatsapp_business_profile") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[{"about":"","websites":[],"messaging_product":"whatsapp"}]}`))
		case strings.HasSuffix(r.URL.Path, "/whatsapp_business_profile"):
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			_, _ = w.Write([]byte(`{"verified_name":"Hadia","name_status":"PENDING_REVIEW","quality_rating":"GREEN"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (g *fakeProfileGraph) snapshot() ([]string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...), append([]string(nil), g.auth...)
}

func newProfileHandler(t *testing.T, graph string, credsErr error) (*WhatsAppProfileHandler, *[]string) {
	t.Helper()
	t.Setenv("META_GRAPH_API_VERSION", "") // the default version, v26.0
	var mu sync.Mutex
	asked := &[]string{}
	creds := func(_ context.Context, tenantID string) (profile.Credentials, error) {
		mu.Lock()
		*asked = append(*asked, tenantID)
		mu.Unlock()
		if credsErr != nil {
			return profile.Credentials{}, credsErr
		}
		return profile.Credentials{PhoneNumberID: "P-" + tenantID, AccessToken: "tok-" + tenantID}, nil
	}
	brand := func(_ context.Context, tenantID string) (profile.Branding, error) {
		return profile.Branding{Name: tenantID, Email: "hello@" + tenantID + ".example"}, nil
	}
	svc := profile.New(creds, func(context.Context) (string, error) { return "APP1", nil }, brand, zap.NewNop())
	svc.GraphURL = graph
	svc.ImageClient = http.DefaultClient
	return NewWhatsAppProfileHandler(svc, zap.NewNop()), asked
}

func pngBytes(t *testing.T, edge int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, edge, edge))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWhatsAppProfileGet(t *testing.T) {
	g := &fakeProfileGraph{}
	h, asked := newProfileHandler(t, g.server(t).URL, nil)
	w := serveAs(h.Get, http.MethodGet, "/api/v1/settings/whatsapp/profile", "", "t1", false, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var p profile.Profile
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.DisplayName != "Hadia" || p.NameStatus != "PENDING_REVIEW" || p.QualityRating != "GREEN" {
		t.Fatalf("profile = %+v", p)
	}
	if len(*asked) != 1 || (*asked)[0] != "t1" {
		t.Fatalf("creds asked for %v", *asked)
	}
	if strings.Contains(w.Body.String(), "tok-t1") {
		t.Fatal("response leaks the access token")
	}
}

func TestWhatsAppProfileUpdate(t *testing.T) {
	g := &fakeProfileGraph{}
	h, _ := newProfileHandler(t, g.server(t).URL, nil)
	w := serveAs(h.Update, http.MethodPut, "/api/v1/settings/whatsapp/profile", `{"about":"We bake","websites":["hadia.example"]}`, "t1", false, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	calls, _ := g.snapshot()
	if !contains(calls, "POST /v26.0/P-t1/whatsapp_business_profile") {
		t.Fatalf("calls = %v", calls)
	}

	w = serveAs(h.Update, http.MethodPut, "/api/v1/settings/whatsapp/profile", `{"about":"`+strings.Repeat("a", 200)+`"}`, "t1", false, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("too long about: status %d, want 400", w.Code)
	}
}

func TestWhatsAppProfileMetaErrorPassesThrough(t *testing.T) {
	g := &fakeProfileGraph{fail: "/whatsapp_business_profile"}
	h, _ := newProfileHandler(t, g.server(t).URL, nil)
	w := serveAs(h.Get, http.MethodGet, "/api/v1/settings/whatsapp/profile", "", "t1", false, "")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "Parameter value is not valid") {
		t.Fatalf("status %d body %s, want 502 with Meta's reason", w.Code, w.Body.String())
	}
}

func TestWhatsAppProfilePhotoMultipartAndURL(t *testing.T) {
	g := &fakeProfileGraph{}
	h, _ := newProfileHandler(t, g.server(t).URL, nil)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "logo.png")
	_, _ = fw.Write(pngBytes(t, 300))
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/settings/whatsapp/profile/photo", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r = r.WithContext(httpware.WithTenantID(r.Context(), "t1"))
	w := httptest.NewRecorder()
	h.Photo(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("multipart status %d: %s", w.Code, w.Body.String())
	}
	calls, auth := g.snapshot()
	if !contains(calls, "POST /v26.0/APP1/uploads") || !contains(calls, "POST /v26.0/upload:S1") {
		t.Fatalf("calls = %v", calls)
	}
	for i, c := range calls {
		if strings.Contains(c, "/upload:") && auth[i] != "OAuth tok-t1" {
			t.Fatalf("upload used %q", auth[i])
		}
	}

	img := pngBytes(t, 300)
	logo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(img) }))
	t.Cleanup(logo.Close)
	w = serveAs(h.Photo, http.MethodPost, "/api/v1/settings/whatsapp/profile/photo", `{"url":"`+logo.URL+`/l.png"}`, "t1", false, "")
	if w.Code != http.StatusOK {
		t.Fatalf("url status %d: %s", w.Code, w.Body.String())
	}

	w = serveAs(h.Photo, http.MethodPost, "/api/v1/settings/whatsapp/profile/photo", `{}`, "t1", false, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no photo: status %d, want 400", w.Code)
	}
}

func TestWhatsAppProfileSyncBranding(t *testing.T) {
	g := &fakeProfileGraph{}
	h, _ := newProfileHandler(t, g.server(t).URL, nil)
	w := serveAs(h.SyncBranding, http.MethodPost, "/api/v1/settings/whatsapp/profile/sync-branding", "", "t1", false, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var rep struct {
		Filled  []string        `json:"filled"`
		Profile profile.Profile `json:"profile"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if !contains(rep.Filled, "email") {
		t.Fatalf("filled = %v", rep.Filled)
	}
}

// A tenant user can never reach another tenant's number through X-Tenant-ID; a platform owner may.
func TestWhatsAppProfileTenantIsolation(t *testing.T) {
	g := &fakeProfileGraph{}
	srv := g.server(t)
	h, asked := newProfileHandler(t, srv.URL, nil)

	if w := serveAs(h.Get, http.MethodGet, "/api/v1/settings/whatsapp/profile", "", "t1", false, "t2"); w.Code == http.StatusOK {
		t.Fatalf("a tenant user naming another tenant got 200: %s", w.Body.String())
	}
	for _, call := range []struct {
		fn     http.HandlerFunc
		method string
		body   string
	}{
		{h.Get, http.MethodGet, ""},
		{h.Update, http.MethodPut, `{"about":"x"}`},
		{h.SyncBranding, http.MethodPost, ""},
	} {
		r := httptest.NewRequest(call.method, "/api/v1/settings/whatsapp/profile", bytes.NewBufferString(call.body))
		r.Header.Set("X-Tenant-ID", "t2")
		r = r.WithContext(httpware.WithTenantID(r.Context(), "t1"))
		rec := httptest.NewRecorder()
		call.fn(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status %d: %s", call.method, rec.Code, rec.Body.String())
		}
	}
	for _, id := range *asked {
		if id != "t1" {
			t.Fatalf("a tenant user reached tenant %q through X-Tenant-ID", id)
		}
	}
	calls, _ := g.snapshot()
	for _, c := range calls {
		if strings.Contains(c, "P-t2") {
			t.Fatalf("called %s for another tenant", c)
		}
	}

	h, asked = newProfileHandler(t, srv.URL, nil)
	w := serveAs(h.Get, http.MethodGet, "/api/v1/settings/whatsapp/profile", "", "platform", true, "t2")
	if w.Code != http.StatusOK || len(*asked) != 1 || (*asked)[0] != "t2" {
		t.Fatalf("platform owner acting for t2: status %d, creds asked for %v", w.Code, *asked)
	}
}

func TestWhatsAppProfileWithoutConnectedNumber(t *testing.T) {
	h, _ := newProfileHandler(t, "http://unused", errors.New("no WhatsApp number is connected for this business"))
	w := serveAs(h.Get, http.MethodGet, "/api/v1/settings/whatsapp/profile", "", "t1", false, "")
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "connected") {
		t.Fatalf("status %d body %s, want 400 saying what to connect", w.Code, w.Body.String())
	}
	w = serveAs(h.Get, http.MethodGet, "/api/v1/settings/whatsapp/profile", "", "", false, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("no tenant: status %d, want 400", w.Code)
	}
}

// After a successful signup the profile is filled from branding in the background, next to the
// template sync; a failed signup starts neither.
func TestEmbeddedSignupStartsProfileAutoSync(t *testing.T) {
	g := &fakeGraph{}
	srv := g.server(t)
	h, _ := newSignupHandler(t, srv.URL, fullCreds)
	started := make(chan string, 1)
	h.WithProfileAutoSync(func(ctx context.Context, tenantID string) {
		if ctx.Err() != nil {
			t.Errorf("profile auto-sync context already done: %v", ctx.Err())
		}
		started <- tenantID
	})
	if w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	select {
	case id := <-started:
		if id != "tenant-1" {
			t.Fatalf("profile auto-sync for %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("profile auto-sync was not started after a successful signup")
	}
}

func TestEmbeddedSignupNoProfileAutoSyncOnFailure(t *testing.T) {
	for _, fail := range []string{"/v26.0/oauth/access_token", "/v26.0/W1/subscribed_apps", "/v26.0/P1/register"} {
		g := &fakeGraph{failPath: fail}
		srv := g.server(t)
		h, _ := newSignupHandler(t, srv.URL, fullCreds)
		started := make(chan string, 1)
		h.WithProfileAutoSync(func(_ context.Context, tenantID string) { started <- tenantID })
		if w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`); w.Code == http.StatusOK {
			t.Fatalf("%s failing still answered 200", fail)
		}
		select {
		case id := <-started:
			t.Fatalf("profile auto-sync started for %q although %s failed", id, fail)
		case <-time.After(100 * time.Millisecond):
		}
	}

	g := &fakeGraph{}
	srv := g.server(t)
	h, _ := newSignupHandler(t, srv.URL, fullCreds)
	h.save = func(context.Context, string, []signupSetting) error { return errors.New("db down") }
	started := make(chan string, 1)
	h.WithProfileAutoSync(func(_ context.Context, tenantID string) { started <- tenantID })
	if w := postSignup(h, `{"code":"c","waba_id":"W1","phone_number_id":"P1"}`); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", w.Code)
	}
	select {
	case <-started:
		t.Fatal("profile auto-sync started although the number was not saved")
	case <-time.After(100 * time.Millisecond):
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
