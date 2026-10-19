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
	// A caller whose own tenant is the platform tenant (service key) may name a tenant too.
	svc := reqWith(t, "platform", false)
	if got := sendingTenant(svc, "tenant-b", "platform"); got != "tenant-b" {
		t.Fatalf("platform-tenant caller may name a tenant, got %q", got)
	}
	if got := sendingTenant(r, "", "platform"); got != "platform" {
		t.Fatalf("got %q", got)
	}
}
