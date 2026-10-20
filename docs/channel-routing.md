# Channel Routing Strategy

## Goals

- Deliver messages via the most reliable, cost-effective provider per tenant/channel
- Provide automatic failover when providers degrade
- Respect tenant preferences, quiet hours, and compliance rules

## Routing Inputs

- **Tenant configuration** – preferred providers, SLAs, regulatory constraints
- **Provider health** – latency, success rate, error codes
- **Message metadata** – priority, category (transactional vs marketing), locale
- **Cost model** – per-channel pricing to optimise spend

## Decision Flow

1. Validate tenant preferences and opt-in status
2. Apply routing matrix (tenant override → global default)
3. Check provider health metrics (latency, error rate)
4. Select primary provider and fallback list
5. Enqueue delivery job with retries + exponential backoff

## Failover Policy

- Threshold-based failover triggered when error rate or latency exceeds SLO
- Automatic fallback to secondary provider; record incident for analytics
- Dead letter queue for messages that exhaust all providers

## Email verification gate

The email worker drops a recipient only when all of these hold (`unverifiedUserRecipients` in `cmd/worker/main.go`):

- the address belongs to a synced user row with `email_verified = false`;
- that user holds at least one role outside the customer-facing set (customer, student, parent, driver, member, mirroring auth-api `selfServiceRoles`), recorded from auth events in `users.metadata.auth_roles` (`internal/modules/identity/auth_roles.go`);
- the template is not exempt (`verificationExemptTemplate`: auth welcome, password reset, OTP, verify, and the whole `digitika/` family).

So customers, learners, parents, riders, people with no user row, and any account whose roles were never recorded are never gated; their mail is sent. `users.email_verified` is persisted on every sync (it was read but never written before 2026-10-20, so every synced user looked unverified). Roles are recorded when the next auth.user.created or updated event arrives, so a long-idle staff account is ungated until then.

## Rate Limiting & Idempotency

- Redis counters limit messages per tenant/channel/time window
- Idempotency keys prevent duplicate sends when retried by clients
- Back-pressure applied when queues exceed thresholds

## Future Enhancements

- Machine learning scoring for provider selection
- Real-time cost optimisation using historical spend
- Tenant-facing routing rules editor in UI
