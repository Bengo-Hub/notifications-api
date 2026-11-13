package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"go.uber.org/zap"
)

// 13 November 2026: truload-backend sends tenant email with a bng_ platform service key (service
// "platform", role superuser). TryDeveloperKey kept only the key's tenant id, so the router saw no
// privileged claims and pinned every send to codevertex: Kura's and Hadia's mail went out through
// the platform account. The key's identity now reaches the request as claims.
func TestDeveloperKeyCarriesItsIdentity(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Header.Get("X-API-Key") {
		case "bng_platform":
			_, _ = w.Write([]byte(`{"client_id":"c1","tenant_id":"t-codevertex","tenant_slug":"codevertex","roles":["superuser"],"service":"platform","environment":"production"}`))
		case "bng_tenant":
			_, _ = w.Write([]byte(`{"client_id":"c2","tenant_id":"t-acme","tenant_slug":"acme","roles":["developer"],"environment":"production"}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer auth.Close()
	a := NewDeveloperKeyAuth(auth.URL, zap.NewNop())

	claimsFor := func(key string) *authclient.Claims {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/notifications/messages", nil)
		r.Header.Set("X-API-Key", key)
		ctx, isDev := a.TryDeveloperKey(httptest.NewRecorder(), r)
		if !isDev || ctx == nil {
			t.Fatalf("%s not handled as a developer key", key)
		}
		c, ok := authclient.ClaimsFromContext(ctx)
		if !ok || c == nil {
			t.Fatalf("%s: no claims on the request", key)
		}
		return c
	}

	p := claimsFor("bng_platform")
	if !p.IsService || !p.IsPlatformOwner || p.TenantID != "t-codevertex" || p.Subject != "c1" {
		t.Fatalf("platform service key lost its identity: %+v", p)
	}
	ten := claimsFor("bng_tenant")
	if ten.IsService || ten.IsPlatformOwner || ten.TenantID != "t-acme" {
		t.Fatalf("an ordinary developer key must stay a plain tenant caller: %+v", ten)
	}
}
