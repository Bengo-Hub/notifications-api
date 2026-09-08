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
//     JWT-derived tenant as a last resort.
//   - Regular tenant: always the tenant ID embedded in their own JWT claims. X-Tenant-ID/tenantId
//     are never honored for a non-platform-owner caller — a client-supplied header or query
//     param is not a security boundary on its own, and honoring it unconditionally (as this
//     helper used to) let any authenticated tenant user read or act on another tenant's data
//     simply by setting a header, across every handler that calls this helper (billing,
//     analytics, WhatsApp inbox/subscriptions, backups, preferences, RBAC, settings...). Fixed
//     2026-09-08 after an audit found tenant-scoped WhatsApp Inbox conversations were reachable
//     cross-tenant this way.
//
// Mirrors subscriptions-api's identical helper (internal/http/handlers/tenant.go) — that helper
// has the same header-trust gap and should get the same fix.
func resolveActingTenantID(r *http.Request) string {
	ctx := r.Context()

	if httpware.IsPlatformOwner(ctx) {
		if h := r.Header.Get("X-Tenant-ID"); h != "" {
			return h
		}
		if q := r.URL.Query().Get("tenantId"); q != "" {
			return q
		}
	}
	return httpware.GetTenantID(ctx)
}
