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

## Paid channel gate (SMS and WhatsApp)

A tenant-scope SMS or WhatsApp message is only queued when the sender may pay for it. Every producer
(event consumers, the HTTP enqueue, broadcasts, the inbox) publishes through `messaging.Publish`,
which asks the installed `messaging.ChannelGate` first. The gate is `credits.SendGate`
(`internal/modules/credits/send_gate.go`), set once in the API (`internal/app/app.go`) and the
worker (`cmd/worker/main.go`):

- it calls subscriptions-api `GET /api/v1/s2s/credits/{sms|whatsapp}/check` (never charges) once per
  tenant and service and keeps the answer for a minute, so a purchase takes effect within a minute;
- a refusal (`no_subscription`, `service_not_activated`, `insufficient_credits`, `overage_cap`)
  drops the message: `Publish` returns no error, so consumers do not retry, and nothing is queued,
  rendered or written to `delivery_logs`;
- platform-scope sends, email and push are never asked; an unreachable subscriptions-api or an
  unknown tenant leaves the decision to the worker;
- broadcasts close a blocked channel in one update (`skipChannel`): its waiting recipients become
  "not sent" with the reason, instead of being queued one by one.

The worker still charges each send (`cc.Consume`, fail closed). If a balance runs out between the
gate and the send, that refusal is a quiet skip (`skippedUnpaid`): it is recorded on a broadcast
recipient but never as a `delivery_logs` row. Before 2026-10-31 every refused attempt was queued,
rendered and logged as `skipped` (boi alone logged 734 SMS refusals in a month); those rows were
deleted.

POS sale receipts (`pos.sale.notification_requested`) go by email or WhatsApp only (`receiptChannel` in
`internal/modules/events/subscriber.go`; pos-api resolves the same way and refuses unpaid WhatsApp with 402 before
publishing). The other POS mappings in `cmd/worker/pos_consumer.go` are still SMS only and need email and approved
WhatsApp templates before they reach customers.

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
