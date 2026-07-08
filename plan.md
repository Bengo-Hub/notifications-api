# Notifications Service Delivery Plan

## Vision & Mandate
- Provide a reusable, low-latency communications platform for all Codevertex products (ordering-service, ERP, billing, etc.) handling transactional and marketing messages across email, SMS, and push channels.
- Centralize template management, provider orchestration, compliance logging, and user preference management while exposing simple APIs and event-driven interfaces.
- **Entity Ownership**: This service owns all notification entities: templates, delivery logs, provider settings, tenant branding, user preferences, and campaigns. All other services call notifications-api APIs to send messages but never store notification delivery state. Services emit events (e.g., `invoice_created`, `order.ready`) consumed by notifications-api. See **shared-docs/CROSS-SERVICE-DATA-OWNERSHIP.md** for the canonical ownership matrix.

## Technical Foundations
- **Language & Framework:** Go 1.22, Hexagonal architecture, Gin/Fiber HTTP layer, gRPC gateway for high-throughput integrations.
- **Data Stores:** PostgreSQL (templates, logs, tenant/organisation configs), Redis (rate limiting, short-lived tokens), S3-compatible object storage for rich assets, ClickHouse or BigQuery export for analytics (future).
- **Queue/Eventing:** NATS JetStream or Kafka for inbound events, AWS SQS-compatible adapter for managed deployments.
- **Infrastructure:** Containerized with Docker, Helm charts for k8s, GitHub Actions CI/CD, Terraform modules for cloud provisioning, OpenTelemetry instrumentation, Prometheus metrics.
- **Security:** mTLS between services, JWT-based client auth, secret rotation via Vault, per-tenant API keys, signed webhooks, organisation/branch-aware RBAC.
- **RBAC:** No local roles/permissions store. **Identity and roles are sourced from auth-api** (GET `/api/v1/auth/me`). Frontends (notifications-ui) use TanStack Query with TTL to cache /me; nav and route protection use returned roles/permissions. API authorization: JWT validation via `shared-auth-client`; no per-endpoint role checks in this service.
- **Redis:** Used for rate limiting, idempotency (24h TTL), and short-lived tokens. Health check validates Postgres, Redis, and NATS.
- **Events:** NATS JetStream for queue workers; outbox pattern via `shared-events` for reliable publishing. Subject: `notifications.events`.
- **Auth-Service SSO Integration:** ✅ **COMPLETED** - Integrated `shared/auth-client` v0.1.0 library for production-ready JWT validation using JWKS from auth-service. Replaced custom JWT validator with production-ready JWKS-based validation. All protected `/v1/{tenantId}` routes require valid Bearer tokens. Falls back to API key auth if JWT not configured. Swagger documentation updated with BearerAuth security definition. **Deployment:** Uses monorepo `replace` directives with versioned dependency (`v0.1.0`). Go workspace (`go.work`) handles local development automatically. Each service has independent DevOps workflows and can be deployed separately while sharing the auth library. See `shared/auth-client/DEPLOYMENT.md` and `shared/auth-client/TAGGING.md` for details.

## Runtime Ports
- **Local development:** default HTTP port **4002** to stay distinct from Food Delivery backend (4000) and Treasury service (4001).
- **Cloud deployment:** `NOTIFICATIONS_HTTP_PORT` is set to **4000** via environment variables so all backend workloads expose a unified ingress port.

## Core Feature Pillars
1. **Tenancy & Org Management (Priority 1)**
   - Multi-organisation and multi-branch support with isolated data domains, branding overrides, and onboarding APIs for external apps to create/link tenants.
   - Per-organisation preferences, quota policies, audit trails, and billing metrics.
3. **Credit-Based SMS/WhatsApp Billing (Priority 1)**
   - Tracks credit balances per tenant for SMS and WhatsApp channels.
   - Integration with **treasury-api** for credit purchases via shared pay links.
   - Pre-send validation: prevents sending if balance is insufficient.
   - Platform Admin: Set SMS/WhatsApp credit rates and overrides.
   - Tenant Admin: View balance and top-up via Treasury.
4. **Template & Content Management (Priority 1)**
   - Versioned templates for email/SMS/push, MJML or Go templating with localization support.
   - Media asset library with CDN links, preview sandbox, approval workflow.
3. **Channel Providers & Routing (Priority 1)**
   - Email: SendGrid primary, SMTP fallback. Platform config at `/api/v1/platform/providers`; credentials encrypted at rest when `NOTIFICATIONS_ENCRYPTION_KEY` is set.
   - SMS: Twilio, Africa’s Talking, configurable custom provider adapters.
   - **Provider test:** `POST /api/v1/platform/providers/{id}/test` with body `{"to": "email or phone"}` sends a test message (SMTP, SendGrid, Africa's Talking).
   - Push: Firebase Cloud Messaging, Apple Push, Web Push.
   - Smart routing with health-based failover, cost-aware routing matrix, provider credential vaulting.
4. **Delivery Orchestration (Priority 2)**
   - Unified message API (REST/gRPC) accepting templated payloads and dynamic data.
   - Bulk campaign support with batching, throttling, personalization tokens, schedule windows.
   - Idempotency handling, per-channel rate limiting, delivery status callbacks.
5. **User Preferences & Compliance (Priority 2)**
   - Subscription center APIs, per-channel opt-in/opt-out, double opt-in for email/SMS.
   - GDPR/Kenya DPA consent recording, audit logs, suppression lists, bounce handling.
6. **Event Ingestion (Priority 3)**
   - Webhooks or event bus consumers for system triggers (order events, payment updates, invoice lifecycle events).
   - Mapping layer (event type → template + channel strategy), dynamic audience segmentation, invoice-specific workflows (send, reminder, receipt).
7. **Observability & Insights (Priority 3)**
   - Real-time dashboards (Grafana) for send volume, success rate, provider latency.
   - Delivery analytics APIs, campaign performance reporting, anomaly detection alerts.
8. **Administration & Self-Service (Priority 4)**
   - Admin UI (optional SPA) for template authoring, provider health checks, key management.
   - Audit trail exports, incident replay tooling, sandbox mode for UAT.

## API & Integration Strategy
- **REST Endpoints:** `/v1/{organisationId}/{branchId}/messages/send`, `/templates`, `/preferences`, `/providers`, `/reports`.
- **gRPC Services:** `NotificationService.Send`, `CampaignService.Schedule`, `PreferenceService`, `TenancyService`.
- **Webhooks:** Delivery receipts, bounce events, provider status updates. Signed with shared secrets.
- **Treasury Integration:** Consume `invoice_created`, `invoice_due`, `payment_link_generated` events to dispatch branded emails/SMS with pay-now links and attachments; publish engagement metrics back to treasury.
- **SDKs:** Provide Go/TypeScript SDKs wrapping authentication, retries, idempotency.
- **Event Contracts:** JSON schema for inbound events (`order_created`, `payment_failed`, `loyalty_awarded`) stored in schema registry.

## Non-Functional Requirements
- P99 send latency < 1s for transactional messages, 99.95% availability target.
- Multi-tenant isolation with organisation and branch scopes, configurable SLAs, rate-limiting per client and channel.
- Disaster recovery with active-active region support, fallback provider strategy.
- Comprehensive testing: unit (Go test), integration (Testcontainers), contract tests, chaos experiments for provider failure.

## Roadmap & Sprints (Priority Order)
1. **Sprint 0 – Foundations (Week 1)**
   - [x] Repository setup
   - [x] CI/CD pipeline with centralized `devops-k8s` workflows
   - [x] Base project structure and observability (health, metrics)
2. **Sprint 1 – Tenancy & Provider Core (Weeks 2-3)**
   - [ ] Organisation/branch data model
   - [ ] Onboarding APIs
   - [x] Provider credential storage (envconfig)
   - [x] Basic send API (`POST /v1/{tenantId}/notifications/messages`)
   - [x] SendGrid + Twilio provider adapters (initial integration)
   - [x] Filesystem templates with ready-to-use samples (email/sms/push)
   - [ ] Template CRUD & localization
3. **Sprint 2 – Delivery Engine & Idempotency (Weeks 4-5)**
   - [x] NATS JetStream queue workers
   - [x] Idempotency with Redis (24h TTL)
   - [ ] Rate limiter
   - [ ] Delivery status tracking
   - [ ] Webhook dispatcher
4. **Sprint 3 – Preferences & Compliance (Weeks 6-7)**
   - [ ] Subscription APIs
   - [ ] Suppression lists
   - [ ] Double opt-in workflows
   - [ ] Consent logging, audit APIs
5. **Sprint 4 – Multi-Provider & Campaigns (Weeks 8-9)**
   - [ ] Africa’s Talking, custom SMS interface
   - [ ] Mailgun/SMTP fallback
   - [ ] Batch scheduling, throttling, segmentation filters
6. **Sprint 5 – Treasury & Invoice Workflows (Weeks 10-11)**
   - [ ] Event ingestion for invoice lifecycle
   - [ ] Dunning automation and attachments
   - [ ] Tenant-specific branding overrides
7. **Sprint 6 – Event Ingestion & Integrations (Weeks 12-13)**
   - [ ] Consumers for broader ecosystem and contract tests
   - [ ] Go/TS SDK updates
8. **Sprint 7 – Observability & Admin (Weeks 14-15)**
   - [ ] Metrics dashboards and alerting policies
   - [x] Platform admin endpoints: list/configure/update/test/deactivate providers at `/api/v1/platform/providers`; test connection per provider via `POST .../providers/{id}/test`
   - [x] Provider secrets encrypted at rest (AES-256-GCM) when `NOTIFICATIONS_ENCRYPTION_KEY` is set; decrypted in provider config loader when building adapters
   - [ ] Sandbox mode
9. **Sprint 8 – Hardening & Launch (Weeks 16-17)**
   - [ ] Performance tuning, failover drills, security review
   - [ ] Documentation and production readiness review
10. **Sprint 9 – Credit-Based Billing (New)** ✅ Completed 2026-05-22
    - [x] Schema for `TenantCredit` and `CreditTransactions` (Ent + Atlas migration)
    - [x] Treasury integration (create payment intents for credits; `payment.succeeded` consumer credits balance)
    - [x] Pre-send guard: HTTP 402 returned when SMS balance ≤ 0
    - [x] Credit dashboard in notifications-ui (real balance + transaction history via API hooks)
11. **Sprint 10 – WhatsApp Subscription Model + Platform Markup** ✅ Completed 2026-05-22
    - [x] `WhatsAppPlan` + `TenantWhatsAppSubscription` Ent schemas + Atlas migration
    - [x] `PlatformBilling` schema extended: `provider_cost_per_sms`, `provider_cost_per_whatsapp`, `min_markup_percentage`
    - [x] `CreditTransaction` tracks `provider_cost` + `platform_fee` for profit analytics
    - [x] `DeductCredits` now tracks platform profit; markup validation in `UpdatePlatformBilling` handler
    - [x] WhatsApp subscription service: plans, subscribe, activate, cancel, quota check
    - [x] Default plan seeds on startup: Basic (KES 500/100 msgs), Standard (KES 1000/500 msgs), Pro (KES 2000/unlimited)
    - [x] Treasury consumer handles `reference_type=whatsapp_subscription` → activates subscription
    - [x] Enqueue handler: WhatsApp uses subscription + quota check (HTTP 402); SMS uses credit balance
    - [x] New routes: `GET /api/v1/billing/whatsapp/plans` (public), subscription CRUD (auth required)
    - [x] notifications-ui: WhatsApp subscription page with plan comparison cards + TreasuryPaymentModal

## Backlog Ideas
- AI-assisted template writing & subject line scoring, in-app messaging channel, voice/IVR support, sentiment analysis on responses, predictive send-time optimization.

---
**Next Steps:** Align with consuming teams on event schemas (especially treasury invoicing events), finalize provider credentials, and schedule integration testing windows with dependent applications across organisations and branches.

