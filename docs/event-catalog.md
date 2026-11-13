# Event Catalog

## Inbound Events (from other services)

| Subject | Description | Producer |
| ------- | ----------- | -------- |
| `treasury.invoice.due` | Invoice due reminder required | Treasury Service |
| `treasury.payment.success` | Payment confirmed, send receipt | Treasury Service |
| `treasury.report.ready` | A scheduled treasury report was generated; emailed with `finance/report_ready` (seven-day download link) to each schedule recipient | Treasury Service |
| `treasury.tax.deadline_reminder` | A tax filing obligation falls due within three days; emailed with `finance/tax_deadline` to the tenant contact (payload email, else the tenant's contact email) | Treasury Service |
| `treasury.payhero.service_wallet_short` | PayHero refused a payment because an account's service wallet cannot pay the channel fee (sent at most once per account every 6 hours); emailed with `platform/payhero_service_wallet_short` to the platform alert recipients (`PLATFORM_ALERT_EMAILS`), locked so no preference can mute it | Treasury Service |
| `treasury.escrow.reconciliation_shortfall` | The hourly escrow reconciliation (`escrow/reconcile.go`) found a tenant's PayHero wallet holding less than its pots owe and opened an incident (published once per incident). While it is open treasury refuses every Team wallet withdrawal and skips the daily commission sweep, so it pages ops: `platform/escrow_shortfall` by email to `PLATFORM_ALERT_EMAILS`, a one-line summary to the Slack webhook and the ops WhatsApp numbers (`broadcast_service_notice_v1`) in auth-api's `platform_alert_channels`. Payload `incident_id`, `wallet_balance`, `pots_total`, `in_flight`, `shortfall`, `currency`, `as_of`, and for tenants sharing one PayHero root wallet `scope: shared_account`, `vendor_id`, `shared_owed`. The shared account's events (one per tenant on it) are gathered for 10 seconds into one alert naming every tenant. One alert per incident: keyed on `incident_id` (tenant plus day without one), or on the vendor for a shared account, in Redis. Locked. Consumer `internal/modules/events/escrow_shortfall_alert.go`, durable `notif-escrow-reconciliation-shortfall` (new messages only) | Treasury Service |
| `treasury.escrow.reconciliation_recovered` | The wallet covers the pots again and the incident closed (same payload, `shortfall` 0.00): `platform/escrow_recovered` by email to `PLATFORM_ALERT_EMAILS` and to the ops Slack webhook, never WhatsApp. Same gathering and dedupe as the shortfall. Locked. Durable `notif-escrow-reconciliation-recovered` (new messages only) | Treasury Service |
| `billing.balance.low` | A credit balance crossed its low mark or reached zero (shared-events `balancealert`). Tenant scope (subscriptions-api: AI credits, eTIMS tokens, SMS, WhatsApp, any later credit service): `billing/balance_low` by email and the `credits_balance_low_v1_btn` WhatsApp template (Buy credits button) to the tenant admin. Platform scope (marketflow-ai: fal, Higgsfield, ElevenLabs): `billing/provider_balance_low` by email to `PLATFORM_ALERT_EMAILS` and `provider_balance_low_v1_btn` (Top up button) to the platform owner's billing phone. Consumer `cmd/worker/balance_low_consumer.go`, durable `notifications-billing-balance-low` on the `subscription` stream, with `processed_events` idempotency | subscriptions-api, marketflow-ai |
| `food.orders.status.changed` | Order status update for customer push/SMS | Food Delivery Backend |
| `erp.payroll.generated` | Payroll notification for employees | ERP System |
| `digitika.enrollment.confirmed` | Enrollment confirmed; `digitika/enrollment_confirmed` to the learner and any guardian | codevertex-website |
| `digitika.payment.succeeded` | Installment received; `digitika/installment_receipt`, or the paid in full WhatsApp variant `digitika/payment_complete` when no balance remains | codevertex-website |
| `digitika.installment.reminder` | Installment due within 3 days or overdue (daily cron; overdue every 3 days); `digitika/installment_reminder`, overdue WhatsApp variant `digitika/installment_overdue` | codevertex-website |
| `digitika.account.created` | Student or parent portal account created; `digitika/portal_account` (temporary password by email only) | codevertex-website |
| `digitika.access_code.requested` | Parent portal one-time code; email `auth/otp_verification`, WhatsApp `auth_otp` from the platform number, never gated | codevertex-website |
| `digitika.contact.received` | Website contact form enquiry. The visitor gets `digitika/contact_form_reply` by email and the `codevertex_enquiry_received_v1_btn` WhatsApp template (phone is required on the form; payload `serviceLabel` names the service). The team gets `platform/website_enquiry` by email only at `PLATFORM_ALERT_EMAILS`, locked so no preference can mute it | codevertex-website |
| `digitika.certificate.issued` | Course certificate issued; `digitika/certificate_issued` to the learner and any guardian | codevertex-website |
| `hadia.gift.received` | A gift reached a Hadia registry. The guest gets `hadia/gift_receipt` when their phone is known. The owner gets `hadia/gift_owner_alert` at once when `owner_digest` is `instant` (or absent); when it is `daily` the alert is held in `digest_items` (one row per event id) and the owner gets one `hadia/owner_gift_digest` email and one `hadia_owner_gift_digest_v1_btn` WhatsApp a day (see below). Consumer `cmd/worker/hadia_consumer.go` | hadia-api |
| `hadia.payout.number_needed` | A registry closed at expiry with money still owed and its owner has no verified payout number past its hold (once per registry). The owner gets `hadia/payout_number_needed` by email and the `hadia_payout_number_needed_v1_btn` WhatsApp template (button: the registry's payouts page, `owner_url`), essential. The same close opens a high priority case, which `hadia.case.opened` already sends to Hadia's staff, so nothing more goes to staff. Consumer `cmd/worker/hadia_consumer.go` | hadia-api |
| `hadia.ops.health_daily` | Hadia's daily health report (responders `platform_ops`). Platform ops path, as for `treasury.gateway.degraded`: `platform/hadia_health_daily` by email to `PLATFORM_ALERT_EMAILS` and a one-line summary to the Slack webhook in auth-api's `platform_alert_channels`; the ops WhatsApp numbers get it too when something needs a person (registries differing, failed payouts, urgent or overdue cases, failed outbox events, or an outbox event unsent for ten minutes). Locked. Consumer `internal/modules/events/hadia_ops_health.go`, durable `notif-hadia-ops-health-daily` (new messages only) | hadia-api |
| `hadia.ops.health_weekly` | Hadia's weekly health report (responders `platform_ops`): `platform/hadia_health_weekly` by email to `PLATFORM_ALERT_EMAILS` and a summary to the ops Slack webhook, never WhatsApp. Locked. Consumer `internal/modules/events/hadia_ops_health.go`, durable `notif-hadia-ops-health-weekly` (new messages only) | hadia-api |
| `project.qa.invite.created` | An address was invited (or re-invited) to a QA acceptance session. `qa/invite` by email to the invited address (`email`, `name`), with the session title, the organisation (Codevertex for `scope: platform`, else the tenant's name), the role and what it does, and the `/qa/{code}` link (`invite_link`, built from projects-api `QA_UI_BASE_URL`; without it the email says the owner will send the link). Platform scope sends from Codevertex, tenant scope with the tenant's sender and branding. Essential. The address never appears in logs | projects-api |
| `project.qa.result.failed` | The first severity 1 or 2 failure of a criterion, or a defect raised by hand. `scope: platform` (a Codevertex service session): ops path, `platform/qa_result_failed` by email to `PLATFORM_ALERT_EMAILS` and a one-line summary to the Slack webhook, plus the ops WhatsApp numbers (`broadcast_service_notice_v1`) for severity 1 only; locked. `scope: tenant` (or none): `qa/result_failed` by email to the session owner (local user table by `owner_id`) and the tenant's contact email from auth-api (owner and admins from auth-api's reach list when there is none), never to ops; essential. Payload `criterion_title`, `area_code`, `requirement_ref`, `feature_code`, `severity`, `result_id`, `defect_id`, `ticket_number`, `ticket_status`, `tester_hint` (masked, email only), `raised_by_hand` | projects-api |
| `project.qa.session.closed` | Testing ended and the report froze. `qa/session_closed` by email to the session owner and any `signers` (`[{email, name}]`, read when projects-api sends them); a tenant session with neither reachable goes to the tenant's contact. Payload `accepted` (the pass rule), `score`, `document_hash`. `scope: platform` also goes to ops by email and Slack (`platform/qa_session_update`, locked), never WhatsApp. Essential | projects-api |
| `project.qa.session.accepted`, `project.qa.session.rejected` | The decision on a closed session. `qa/session_decided` to the owner and signers as for close, with `reason` and `document_hash`; platform scope also to ops by email and Slack (`platform/qa_session_update`). Essential. All five `project.qa.*` events: consumer `internal/modules/events/project_qa.go` on stream `projects`, durable `notifications-projects-qa` (all messages); each event id is claimed in Redis for 7 days (in the process without Redis) and every email carries the idempotency key `project-qa-<event id>`, so a redelivery sends nothing | projects-api |

### Daily summaries (`cmd/worker/hadia_digest.go`, N3, 2026-11-06)

The scheduled notifier (`cmd/worker/scheduled_notifications.go`) runs the digest job every five
minutes on every pod. From 18:00 Africa/Nairobi (tenant metadata `digest_hour` in auth-api changes
the hour) it sends each owner with held gifts one email and one WhatsApp: the count, the total,
one line per registry and a button to the gift log (`app/registries/<id>/gifts`, or `app` when the
day covers several registries). Guest names and messages are never in it. A day with no gifts
sends nothing.

- A gift received at or after the hour belongs to the next day's summary; one that reaches the
  worker after that day's summary went out waits for the next day.
- Once a day per owner: a `digest_sends` row unique on (tenant, kind, owner key, date) is claimed
  before anything is sent. Idempotency key `digest:{tenant}:{owner key}:{date}`; the owner key is a
  hash of hadia-api's `owner_id`, never the address.
- Crash safe: each queued channel is written on the row, so after the claim's ten minute lease the
  next run finishes only the missing channel, with the items it started with.
- Opt-out: an address on the tenant's suppression list with scope `all` gets nothing on that
  channel. A marketing opt-out does not stop it (the summary is transactional).
- Preferences: `hadia/owner_gift_digest` is in the same class as `hadia/gift_owner_alert`
  (optional), so a tenant that turned owner alerts off gets neither.
- Sent summaries and their items are removed after seven days (the items hold the owner's email
  and phone).

Digitika events carry the learner (`studentId`, `studentName`, `studentEmail` real address only,
`studentPhone`) and the guardian (`guardianName`, `guardianEmail`, `guardianPhone`).
`cmd/worker/digitika_consumer.go` turns each into a party notice (`sendToParties`): the learner and
the guardian are each reached on the email and WhatsApp contacts they have, a phone shared by both
gets one message, and a placeholder (`.invalid`) email is never mailed. SMS is not used.

## Outbound Events (emitted by notifications service)

| Subject | Description |
| ------- | ----------- |
| `notifications.delivery.status` | Final outcome (`sent`, or `failed` after the fallback provider) of a message that names its source document. Emitted only for messages whose metadata carries `source_service`, `reference_type` and `reference_id`; payload has `tenant_id`, those three keys, `channel`, `template`, `recipients`, `status`, `request_id` and `error` on failure. Code: `cmd/worker/delivery_status.go` (2026-10-03). Treasury tags `invoice_sent` and dunning reminders and records the outcome on the invoice. |
| `notifications.campaign.completed` | Campaign finished processing |

Planned, not emitted yet: `notifications.delivery.accepted`, `notifications.delivery.failed` and
`notifications.delivery.completed` (provider receipts such as SMS delivered or email opened need
provider webhooks first). Until then `notifications.delivery.status` is the only delivery event.

## Event Fields

Common fields across events:

- `tenantId` – organisation/branch identifier
- `channel` – `email`, `sms`, `push`
- `template` – template identifier used
- `metadata` – map for correlation IDs, campaign IDs
- `retries` – number of attempts for delivery (outbound)

## Schema Governance

- JSON Schemas stored under `docs/schemas/` (to be added)
- Schemas versioned with semantic version; breaking changes require new schema ID
- Schema registry integration planned (e.g., Redpanda/Schema Registry)
