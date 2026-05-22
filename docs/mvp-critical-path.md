# Notifications API - MVP Critical Path

**Last Updated**: May 2026  
**Purpose**: Notifications-api MVP scope; aligns with [shared-docs/mvp-critical-path.md](../../../shared-docs/mvp-critical-path.md).

---

## Notifications API in BengoBox MVP

| Item | Status |
|------|--------|
| **Production domain** | `notificationsapi.codevertexitsolutions.com` |
| **RBAC** | No local roles; JWT from auth-api; shared-auth-client validation |
| **Endpoints** | Templates, platform providers, tenant branding, analytics/delivery, delivery logs, send message, template test send, billing balance/transactions/topup |

---

## MVP Scope

### P0
- [x] Tenant-scoped routes `/api/v1/{tenantId}/...` with JWT validation
- [x] Template list, get, update (PUT); template test send (POST .../templates/{id}/test)
- [x] Platform providers (list, create, update, delete, test)
- [x] Branding GET/PUT per tenant
- [x] Send message API (POST)
- [x] Analytics/delivery stats (real data from delivery_log store)

### P1
- [x] Idempotency (notification handler)
- [x] Credit-based billing — SMS pre-send guard (HTTP 402 on zero balance), treasury top-up flow, credit balance + transaction history API
- [x] WhatsApp subscription model — Basic/Standard/Pro plans, treasury payment intent, quota check per plan, subscription management API + UI
- [x] Platform markup tracking — provider_cost_per_sms, min_markup_percentage, platform_fee logged per CreditTransaction
- [x] Analytics tenantID fix — handlers now extract tenantID from JWT claims when URL param is absent (fixes empty stats for non-platform users)
- [ ] Rate limiting (documented for post-MVP)
- [ ] Provider health and failover
- [x] Delivery log API for UI monitoring (GET /api/v1/analytics/logs)

---

## References

- [Notifications API Plan](../plan.md)
- [Integrations](integrations.md)
- [Shared MVP Critical Path](../../../shared-docs/mvp-critical-path.md)
