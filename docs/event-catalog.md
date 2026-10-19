# Event Catalog

## Inbound Events (from other services)

| Subject | Description | Producer |
| ------- | ----------- | -------- |
| `treasury.invoice.due` | Invoice due reminder required | Treasury Service |
| `treasury.payment.success` | Payment confirmed, send receipt | Treasury Service |
| `treasury.report.ready` | A scheduled treasury report was generated; emailed with `finance/report_ready` (seven-day download link) to each schedule recipient | Treasury Service |
| `treasury.tax.deadline_reminder` | A tax filing obligation falls due within three days; emailed with `finance/tax_deadline` to the tenant contact (payload email, else the tenant's contact email) | Treasury Service |
| `treasury.payhero.service_wallet_short` | PayHero refused a payment because an account's service wallet cannot pay the channel fee (sent at most once per account every 6 hours); emailed with `platform/payhero_service_wallet_short` to the platform alert recipients (`PLATFORM_ALERT_EMAILS`), locked so no preference can mute it | Treasury Service |
| `food.orders.status.changed` | Order status update for customer push/SMS | Food Delivery Backend |
| `erp.payroll.generated` | Payroll notification for employees | ERP System |
| `digitika.enrollment.confirmed` | Enrollment confirmed; `digitika/enrollment_confirmed` to the learner and any guardian | codevertex-website |
| `digitika.payment.succeeded` | Installment received; `digitika/installment_receipt`, or the paid in full WhatsApp variant `digitika/payment_complete` when no balance remains | codevertex-website |
| `digitika.installment.reminder` | Installment due within 3 days or overdue (daily cron; overdue every 3 days); `digitika/installment_reminder`, overdue WhatsApp variant `digitika/installment_overdue` | codevertex-website |
| `digitika.account.created` | Student or parent portal account created; `digitika/portal_account` (temporary password by email only) | codevertex-website |
| `digitika.access_code.requested` | Parent portal one-time code; email `auth/otp_verification`, WhatsApp `auth_otp` from the platform number, never gated | codevertex-website |
| `digitika.contact.received` | Contact form acknowledgement; `digitika/contact_form_reply` by email and, with a phone, WhatsApp | codevertex-website |
| `digitika.certificate.issued` | Course certificate issued; `digitika/certificate_issued` to the learner and any guardian | codevertex-website |

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
