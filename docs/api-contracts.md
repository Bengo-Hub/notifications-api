# API & Event Contracts

## REST Endpoints

- `POST /v1/{tenantId}/notifications/messages`
  - Create notification intent with channel metadata, template reference, personalization data
  - Response: `202 Accepted` with `{ status: "queued", requestId: "..." }`
  - **Rate Limited**: Per-channel daily limits based on subscription plan:
    - `email_notifications_per_day`: Starter=50, Growth=500, Professional=5,000
    - `webhook_calls_per_day`: Starter=100, Growth=1,000, Professional=10,000
  - **Billed per message** (no rate limit): SMS and WhatsApp are charged by subscriptions-api. A tenant needs an active
    subscription with the channel activated (by buying a bundle or, for WhatsApp, a monthly plan). Each message is paid from
    the plan allowance, then bundles, then KES 1 overage billed on the next invoice, and sending pauses when unpaid overage
    reaches the platform cap. The API answers `402` with an `error` of `no_subscription`, `service_not_activated`,
    `insufficient_credits` or `overage_cap`. The platform tenant, the demo tenant and exempt tenants are never charged.
    Event-driven SMS and WhatsApp for a tenant that may not pay are dropped before they are queued and are not logged
    (see `docs/channel-routing.md`, Paid channel gate).
  - Each recipient counts as one unit. Returns `429` with `X-RateLimit-*` headers when exceeded.
  - Sample payloads:

```json
{
  "channel": "email",
  "tenant": codevertex,
  "template": "invoice_due",
  "to": ["customer@example.com"],
  "data": {
    "name": "Jane",
    "invoice_number": "INV-1001",
    "amount": "KES 1,200",
    "due_date": "2025-11-30",
    "payment_link": "https://pay.example.com/invoices/INV-1001",
    "brand_name": codevertex,             // optional overrides; if absent, defaults from DB or tenant slug
    "brand_email": "hello@bengobox.com",  // optional
    "brand_phone": "+254700000000",       // optional
    "brand_logo_url": "https://cdn.example.com/logo.png" // optional
  },
  "metadata": {
    "subject": "Invoice INV-1001 is due",
    "provider": "smtp"
  }
}
```

```json
{
  "channel": "sms",
  "tenant": codevertex,
  "template": "otp",
  "to": ["+254700000000"],
  "data": { "otp": "123456", "ttl_minutes": 5, "brand_name": codevertex },
  "metadata": { "provider": "africastalking" }
}
```

- `GET /v1/{tenantId}/templates`
  - List available templates for tenant (id, channel, locale)

- `GET /v1/{tenantId}/templates/{id}?channel=email|sms|push`
  - Fetch raw template content for preview or client-side rendering

- `GET /api/v1/announcements/active?service=pos` (public)
  - The platform's "what's new" banners that app shows now: active, started, not ended, aimed at
    the service or at every app; highest priority first, then newest. Cached 30s per pod,
    `Cache-Control: public, max-age=60`, any origin allowed (no credentials).
  - Response: `{ "announcements": [{ id, title, summary, highlights[], cta_label, cta_url,
    audience: all|admins, tone: feature|info|warning, priority, dismissible, starts_at, ends_at,
    updated_at, variants? }] }`
  - Rendered by shared-ui-lib's `AnnouncementBanner` (`@bengo-hub/shared-ui-lib/announcements`):
    admin-only ones show to admins, `{orgSlug}` in `cta_url` is substituted, and dismissals are kept
    per viewer on the device. Mounted on the pos-ui and treasury-ui dashboards (2026-10-03).
  - `variants` (from `metadata.variants`, keyed by a flag such as `payhero_active`) holds text for
    viewers in another state: `{ title?, summary, highlights[], cta_label?, cta_url? }`. The app
    passes `flags` to the banner (treasury-ui and pos-ui report `payhero_active` from the tenant's
    PayHero status); a true flag swaps in that variant, false keeps the base text, and a flag still
    loading holds the announcement back. Dismissals are per variant (shared-ui-lib v0.1.101).

- `GET|POST /api/v1/platform/announcements`, `PUT|DELETE /api/v1/platform/announcements/{id}`
  (platform super admin; notifications-ui Platform > Announcements)
  - Create or edit: title, summary, highlights, `cta_label` + `cta_url` (https, `mailto:` or an
    app path), `services` (empty = every app), audience, tone, priority, dismissible, is_active,
    starts_at, optional ends_at, and optional `metadata.variants` (validated like the base text;
    an edit without `metadata` keeps the stored variants).
  - Ended announcements are hard deleted on start and hourly (once fleet-wide via the shared
    `RunOnce`); one with no end runs until switched off or deleted.

All responses include `X-Request-ID` header for traceability. Error responses follow RFC 7807.

## Webhooks

- Providers (SendGrid, Mailgun, Twilio, FCM, APNS) POST to `/webhooks/{provider}` (to be implemented)
- Verify signatures per provider best practices; store events for compliance/audit

### Mail delivery feedback (Stalwart)

The worker marks an email `sent` when the mail server accepts it, which says nothing about the recipient's
server. Stalwart therefore posts a webhook when a delivery fails for good.

- `POST /api/v1/webhooks/stalwart/delivery` is public and authenticated only by the `X-Signature` header, an
  HMAC-SHA256 of the raw body with the shared key, base64 encoded. The current key and the previous one are
  accepted, so a rotation does not drop notices in flight. With no key configured the endpoint answers 503.
- Only `delivery.failed` events are subscribed. A permanent failure (SMTP 5xx or enhanced status 5.x.x) marks
  the delivery log row `bounced`. The row is found by the Message-ID the worker sets (the request id is the
  local part, `<request-id@sending-domain>`); a notice without it marks the newest `sent` email to that
  recipient within 7 days. A failure that says the mailbox or domain does not exist (5.1.x, "user unknown")
  also suppresses the address for 30 days in the Redis list the worker's email guard reads.
- `GET /api/v1/platform/mail-delivery-key` returns `{configured, source, key_fingerprint, updated_at,
  last_event_at, webhook_url}` and never the key. `PUT /api/v1/platform/mail-delivery-key` with
  `{"generate": true}` rotates the key: it is stored (the old one becomes the previous key), pushed to Stalwart
  through email-provisioner (`PUT /internal/delivery-webhook`, which creates or updates the Stalwart webhook,
  only for urls under `DELIVERY_WEBHOOK_URL_PREFIX`) and rolled back if Stalwart does not accept it.
  Platform admins do this from Settings, Security in notifications-ui.
- The key is stored through the same platform key store as the credential encryption key
  (`internal/platformkey`), so both follow one pattern: platform-level service config row, generate, fingerprint,
  no key material in any response.
- Configuration (defaults are the in-cluster addresses): `EMAIL_PROVISIONER_URL`, `MAIL_DELIVERY_WEBHOOK_URL`.

## Events

- **Inbound subjects**: `notifications.events` containing messages from other services (`invoice.due`, `payment.success`)
- **Outbound subjects**: `notifications.delivery.status` (final sent or failed outcome of a correlated message, see event-catalog.md)
- CloudEvents envelope example:

```jsonc
{
  "id": "uuid",
  "source": "treasury/invoice",
  "specversion": "1.0",
  "type": "notifications.message.enqueue",
  "datacontenttype": "application/json",
  "time": "2025-01-02T12:00:00Z",
  "data": {
    "tenantId": "org_123",
    "channel": "email",
    "template": "invoice_due",
    "payload": { "invoiceNumber": "INV-123" }
  }
}
```

## Versioning

- APIs follow `/v1` prefix; breaking changes require new version.
- Event schema evolution uses additive changes; breaking fields require new event type.

## Authentication

- REST endpoints protected with JWT service accounts (Auth Service JWKS). Optional API key fallback.
- Webhooks validated via provider signatures; responses use 2xx for success, 5xx to trigger retries.
