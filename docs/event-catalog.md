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
