# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Maskani consumer (2026-10-08): durable `notifications-maskani` on stream `maskani` (`maskani.>`). Email and WhatsApp (the active channels) for bill issued, payment received, visitor gate code, visitor arrived, walk-in approval, portal invite, sale agreement active and fully paid, to the payload's own email and phone; urgent incidents and work orders past SLA to the tenant contact email and phone; expiring vendor documents by email only. WhatsApp uses ten new UTILITY templates (`maskani_*_v1[_btn]` in `templatesync/templates.json`); links are URL buttons on `https://maskaniapp.codevertexafrica.com/{{1}}` with the suffix `<slug>/<path>`. Email templates in `templates/email/maskani/` (detail table, callout, brand button). Idempotent per event id and channel. Classified under "Property" in the preferences registry. `maskani_consumer_test.go` checks parameter counts against the manifest, button links and template files. Email links use the tenant `service_urls.maskani` or `NOTIFICATIONS_MASKANI_APP_URL`.
- Phone code sign-in (2026-10-08): `auth.user.otp.requested` carrying `phone` and no `email` sends the `auth_otp` AUTHENTICATION template on WhatsApp from the platform number (code as body and copy-code button parameter), so a tenant's WhatsApp plan never blocks sign-in.
- Phone code sign-in, email first (2026-10-08): when the event also carries `login_email` with `channel: "email"`, the code goes by email (`auth/otp_verification`, platform sender, subject "{brand} sign-in code", idempotency keyed on the code so a resend is never swallowed) and WhatsApp is not used. WhatsApp stays the fallback for members with no real email and for an explicit WhatsApp request.
- Maskani bills (2026-10-08) show the breakdown: each charge, with quantity x rate only for metered or rated charges and per-line VAT only on taxed charges; subtotal and VAT rows only when the bill is taxed; the paybill block only when the fund has one. WhatsApp has three bill templates (`maskani_bill_issued_v1_btn` untaxed, `_vat_v1_btn` taxed, `_nopaybill_v1_btn`) because a template cannot hide a line; the charges go on one line (Meta forbids newlines in a parameter), cut with "and N more" past 600 characters.
- After deploy: sync the `maskani_*` templates (notifications-ui Templates > WhatsApp sync, dry run first). Until Meta approves them, WhatsApp is skipped for those messages and email still goes.
- Service-level RBAC: User, Role, Permission Ent schemas with 4 roles (viewer, manager, admin, superuser) and 20 fine-grained permissions
- Identity module with NATS-driven user sync from auth-service and JIT user provisioning from JWT
- Authenticator middleware (RequireAuth, RequireRoles, RequirePermissions) with superuser/admin bypass
- Per-route permission enforcement on all protected endpoints (platform, analytics, templates, billing, settings, notifications)
- Seed command extended to bootstrap roles, permissions, and role-permission mappings
- Atlas versioned migration `add_identity_rbac` for users, roles, permissions, and junction tables
- API Key Authentication fallback when JWT tokens are not provided (via `SECURITY_ENABLE_API_KEY_AUTH`)
- Swagger UI Bearer prefix auto-add for JWT tokens

### Changed
- Bumped shared-auth-client from v0.4.0 to v0.4.1 (adds Permissions field to JWT Claims)
- Migration generator now uses `search_path=ent_dev` for schema isolation (consistent with treasury-api pattern)
- `fix_migration.go` updated to also clear `ent_dev` schema

### Added (previous)
- Initial Go service scaffolding with Gin API, middleware, health endpoints, and documentation
- HTTPS support for local development using mkcert certificates
- Custom Swagger UI handler with protocol-aware URL detection for HTTPS compatibility

### Changed
- Standardized API base path to `/api/v1` (previously `/v1`)
- Standardized Swagger documentation path to `/v1/docs` (previously `/swagger/*`)
- Updated Swagger specifications to support both HTTP and HTTPS schemes
- Swagger UI now automatically detects and uses the correct protocol (HTTP/HTTPS) based on request
- Fixed Swagger UI to correctly load API definition from `/v1/docs/swagger/doc.json`

## [0.2.0] - 2025-11-14

### Added
- Production-ready enqueue endpoint with Redis-backed idempotency
- NATS JetStream publisher and worker consumer
- Filesystem template loader and ready-to-use templates (email/sms/push)
- Initial provider integrations: SendGrid (email), Twilio (sms)
- SSO integration: JWT enforcement via Auth Service (configurable), API key fallback
- Tenant branding support and base email layout (header/footer, CSS)
- Local testing guide and README updates (links to docs, production URL)
- **Auth-Service SSO Integration:** Integrated `shared/auth-client` v0.1.0 library for production-ready JWT validation using JWKS from auth-service. Replaced custom JWT validator with production-ready JWKS-based validation. All protected `/v1/{tenantId}` routes require valid Bearer tokens. Falls back to API key auth if JWT not configured. Swagger documentation updated with BearerAuth security definition. Uses monorepo `replace` directives with versioned dependency. See `shared/auth-client/DEPLOYMENT.md` and `shared/auth-client/TAGGING.md` for details.

### Changed
- Template listing endpoint now reflects actual templates on disk
- Replaced local `replace` directive with Go workspace (`go.work`) for local development; production deployments use private Go module approach.

### DevOps
- Verified centralized `devops-k8s` integration, ingress host set to `notifications.codevertexafrica.com`