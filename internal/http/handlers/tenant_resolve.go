package handlers

import (
	"net/http"

	httpware "github.com/Bengo-Hub/httpware"
)

// resolveActingTenantID resolves which tenant a request should operate on, supporting the
// platform-admin "act as tenant X" pattern (a persistent tenant-switcher in notifications-ui's
// top nav, mirrored from subscriptions-ui's identical mechanism): the frontend sends X-Tenant-ID/
// X-Tenant-Slug for the tenant an admin has selected, so this must be checked BEFORE falling back
// to the admin's own JWT tenant.
//
//   - Platform owner: X-Tenant-ID header first, then the legacy ?tenantId= query param, then the
//     JWT-derived tenant as a last resort (preserves this handler's pre-existing "no explicit
//     override → my own tenant" behavior — every call site already has its own considered
//     behavior for a still-empty result, e.g. falling back to the platform tenant ID; this helper
//     only adds the header check ahead of what already existed, it doesn't remove any prior path).
//   - Regular tenant: X-Tenant-ID header first (defensive; should already match the JWT), then
//     the tenant ID embedded in the JWT claims.
//
// Mirrors subscriptions-api's identical helper (internal/http/handlers/tenant.go), adapted to
// fall back to the JWT tenant rather than "" for a platform owner with no explicit override, to
// match how this service's handlers already behaved before the header check was added.
func resolveActingTenantID(r *http.Request) string {
	ctx := r.Context()

	if h := r.Header.Get("X-Tenant-ID"); h != "" {
		return h
	}
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			return q
		}
	}
	return httpware.GetTenantID(ctx)
}
