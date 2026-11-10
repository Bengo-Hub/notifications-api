package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	httpware "github.com/Bengo-Hub/httpware"
)

func reqWith(t *testing.T, tenantID string, platformOwner bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/messages", nil)
	var seen *http.Request
	h := httpware.TenantV2(httpware.TenantConfig{
		ClaimsExtractor: func(ctx context.Context) (string, string, bool, bool) {
			return tenantID, "", platformOwner, true
		},
		URLParamFunc: func(*http.Request, string) string { return "" },
		Required:     false,
	})(http.HandlerFunc(func(_ http.ResponseWriter, rr *http.Request) { seen = rr }))
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen == nil {
		t.Fatal("tenant middleware did not call next")
	}
	return seen
}

func TestSendingTenantIgnoresBodyForOrdinaryTenants(t *testing.T) {
	r := reqWith(t, "tenant-a", false)
	if got := sendingTenant(r, "tenant-b", "platform"); got != "tenant-a" {
		t.Fatalf("an ordinary tenant must send as itself, got %q", got)
	}
	if got := sendingTenant(r, "", "platform"); got != "tenant-a" {
		t.Fatalf("got %q", got)
	}
}

func TestSendingTenantLetsPlatformNameATenant(t *testing.T) {
	r := reqWith(t, "platform", true)
	if got := sendingTenant(r, "tenant-b", "platform"); got != "tenant-b" {
		t.Fatalf("platform owner may send for a named tenant, got %q", got)
	}
	// A plain member of the platform tenant is not privileged: they send as themselves (security
	// audit 2026-11-10). Service keys are privileged through the router's tenantContext.
	member := reqWith(t, "platform", false)
	if got := sendingTenant(member, "tenant-b", "platform"); got != "platform" {
		t.Fatalf("a plain platform-tenant member must not name another tenant, got %q", got)
	}
	if got := sendingTenant(r, "", "platform"); got != "platform" {
		t.Fatalf("got %q", got)
	}
}
