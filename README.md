# Notifications Service

Multi-channel notifications orchestration platform for Codevertex products. Handles tenant-aware email, SMS, and push messaging, template management, provider routing, and compliance logging.

## Key Features

- Go 1.22 service with Gin HTTP API and event-driven architecture
- Modular providers for SendGrid/Mailgun, Twilio/Africa's Talking, FCM/APNS
- PostgreSQL for templates/audit logs, Redis for idempotency + rate limits, NATS JetStream for event ingestion
- Observability via zap logging, Prometheus metrics, OTEL-ready instrumentation
- Hexagonal architecture with domain-driven modules (tenancy, templates, channels, orchestration)

## Getting Started

```bash
cp config/app.env.example .env
make tidy
make run
```

For step-by-step local setup (Redis + NATS + Postgres), see:
- [docs/local-testing.md](docs/local-testing.md)

### API Documentation

- Swagger UI: `https://notifications.codevertex.local:4002/v1/docs/` (local HTTPS) or `http://localhost:4002/v1/docs/` (local HTTP)
- Regenerate the OpenAPI spec after updating handler annotations, then copy the result over the
  served copy (`swagger.go` embeds `internal/http/handlers/swagger.json`, not the generated
  `internal/http/docs/swagger.json` directly):
  ```bash
  swag init -g cmd/api/main.go -o internal/http/docs --parseDependency
  cp internal/http/docs/swagger.json internal/http/handlers/swagger.json
  ```
  `--parseDependency` is required — without it, swag fails outright on the generic
  `pagination.Response[T]` type used by `templates.go`'s `@Success` annotation (it can't resolve a
  generic struct from an external module without being told to actually parse into dependencies).

Port mapping:

- Local development serves the API on **https://notifications.codevertex.local:4002** (HTTPS with mkcert) or **http://localhost:4002** (HTTP).
- In Kubernetes, the Helm chart overrides `NOTIFICATIONS_HTTP_PORT` to **4000** so all backend workloads expose a unified ingress port.
- Production ingress: `https://notificationsapi.codevertexafrica.com`
- **API Base Path:** All API endpoints are under `/api/v1/`

### Environment Variables

All configuration keys prefixed with `NOTIFICATIONS_`. See [`config/app.env.example`](config/app.env.example) for channel/provider keys and defaults.

Security (SSO via Auth Service):
- `NOTIFICATIONS_REQUIRE_JWT=true`
- `NOTIFICATIONS_JWKS_URL=https://auth.codevertex.local:4101/api/v1/.well-known/jwks.json` (point to Auth Service in your env)
- `NOTIFICATIONS_JWT_ISSUER=https://auth.codevertex.local:4101`
- `NOTIFICATIONS_JWT_AUDIENCE=notifications`
- Optional fallback: `NOTIFICATIONS_API_KEY` for service-to-service without JWT in dev

### Common Commands

| Command | Description |
| ------- | ----------- |
| `make run` | Start HTTP API server |
| `make worker` | Run background worker (JetStream consumers) |
| `make test` | Execute go tests |
| `make lint` | Run golangci-lint (install separately) |
| `make build` | Build API + worker binaries |

## Project Structure

```
cmd/
  api/         # HTTP entrypoint
  worker/      # background consumer entrypoint
internal/
  app/         # bootstrap + lifecycle
  config/      # environment configuration loader
  http/        # Gin handlers & routes
  messaging/   # domain services (channel routing, idempotency)
  providers/   # channel providers (email, sms, push)
  platform/    # infrastructure adapters (database, cache, events, templates)
  shared/      # logger and middleware
```

## Documentation

Detailed docs live under `docs/` and are indexed in [`docs/documentation-guide.md`](docs/documentation-guide.md):

- [`docs/architecture.md`](docs/architecture.md) – service architecture, module boundaries
- [`docs/development-workflow.md`](docs/development-workflow.md) – local setup, CI/CD pipeline
- [`docs/testing-strategy.md`](docs/testing-strategy.md) – testing pyramid, tooling, coverage
- [`docs/channel-routing.md`](docs/channel-routing.md) – provider selection, failover logic
- [`docs/api-contracts.md`](docs/api-contracts.md) – REST/webhook/event schema conventions
- [`docs/local-testing.md`](docs/local-testing.md) – quick local run with Docker

## Community & Governance

- [`CONTRIBUTING.md`](CONTRIBUTING.md)
- [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md)
- [`SECURITY.md`](SECURITY.md)
- [`SUPPORT.md`](SUPPORT.md)
- [`CHANGELOG.md`](CHANGELOG.md)

## Next Steps

- Implement provider SDK integrations and routing matrix
- Add event ingestion workers & rate limiting policies
- Expand template rendering pipeline (MJML/Handlebars) with preview tooling
- Integrate notifications with treasury service for invoice workflows and dunning sequences
