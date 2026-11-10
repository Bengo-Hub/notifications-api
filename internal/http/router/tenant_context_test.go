package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	httpware "github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
)

// Security audit 2026-11-10: any member of the codevertex tenant was marked platform owner (slug
// check), and the handlers let a platform owner act for any tenant through X-Tenant-ID, so a plain
// member there could read and send another tenant's notifications. Only the signed
// is_platform_owner claim, or a service key, is privileged.
func TestTenantContextPrivilege(t *testing.T) {
	var gotPO bool
	h := tenantContext()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPO = httpware.IsPlatformOwner(r.Context())
	}))
	privileged := func(c *authclient.Claims) bool {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), c))
		h.ServeHTTP(httptest.NewRecorder(), r)
		return gotPO
	}
	const own = "11111111-1111-1111-1111-111111111111"
	if privileged(&authclient.Claims{TenantID: own, TenantSlug: "codevertex"}) {
		t.Fatal("a plain codevertex member must not be a platform owner")
	}
	if !privileged(&authclient.Claims{TenantID: own, TenantSlug: "codevertex", IsPlatformOwner: true}) {
		t.Fatal("the signed platform-owner claim is privileged")
	}
	if !privileged(&authclient.Claims{IsService: true}) {
		t.Fatal("a service key is privileged (producers send on behalf of tenants)")
	}
}
