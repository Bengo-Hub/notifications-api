# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
- Verified centralized `devops-k8s` integration, ingress host set to `notifications.codevertexitsolutions.com`