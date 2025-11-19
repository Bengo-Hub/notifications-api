# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **API Key Authentication:** Added support for API key authentication as fallback when JWT tokens are not provided
  - Configure via `NOTIFICATIONS_ENABLE_API_KEY_AUTH=true` (default: true)
  - API keys are sent in `X-API-Key` header
  - API keys are validated against auth-service and cached for 5 minutes
- **Swagger UI Bearer Prefix:** Swagger UI now automatically adds "Bearer " prefix when users paste JWT tokens

### Added
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