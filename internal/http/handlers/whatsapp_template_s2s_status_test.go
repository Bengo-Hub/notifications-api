package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bengobox/notifications-api/internal/whatsapp/tenanttemplates"
)

func TestS2STemplateStatus(t *testing.T) {
	g := &fakeTemplateGraph{}
	srv := g.server(t)
	h, asked := newTenantTemplatesHandler(t, srv.URL, nil)

	w := httptest.NewRecorder()
	h.S2STemplateStatus(w, httptest.NewRequest(http.MethodGet,
		"/api/v1/s2s/whatsapp/templates/status?tenant_id=plat&names=auth_otp,%20mf_video_ready_btn", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got templateStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.TenantID != "plat" || len(got.Templates) != 2 || len(*asked) != 1 || (*asked)[0] != "plat" {
		t.Fatalf("got %+v, asked %v", got, *asked)
	}
	if got.Templates[0].Status != "APPROVED" || got.Templates[1].Status != tenanttemplates.StatusMissing {
		t.Fatalf("statuses %+v", got.Templates)
	}
	if g.posts() != 0 {
		t.Fatal("status must never write to Meta")
	}
}

func TestS2STemplateStatusRefusals(t *testing.T) {
	g := &fakeTemplateGraph{}
	srv := g.server(t)
	h, _ := newTenantTemplatesHandler(t, srv.URL, nil)
	for _, q := range []string{"?names=mf_video_ready_btn", "?tenant_id=plat", "?tenant_id=plat&names=,,"} {
		w := httptest.NewRecorder()
		h.S2STemplateStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/s2s/whatsapp/templates/status"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, w.Code)
		}
	}
	noWABA, _ := newTenantTemplatesHandler(t, srv.URL, errors.New("no number"))
	w := httptest.NewRecorder()
	noWABA.S2STemplateStatus(w, httptest.NewRequest(http.MethodGet, "/api/v1/s2s/whatsapp/templates/status?tenant_id=plat&names=a", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing credentials: status %d, want 400", w.Code)
	}
}
