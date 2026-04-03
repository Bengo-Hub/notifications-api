# API & Event Contracts

## REST Endpoints

- `POST /v1/{tenantId}/notifications/messages`
  - Create notification intent with channel metadata, template reference, personalization data
  - Response: `202 Accepted` with `{ status: "queued", requestId: "..." }`
  - **Rate Limited**: Per-channel daily limits based on subscription plan:
    - `email_notifications_per_day`: Starter=50, Growth=500, Professional=5,000
    - `webhook_calls_per_day`: Starter=100, Growth=1,000, Professional=10,000
  - **Credit-based** (no rate limit): SMS and WhatsApp are gated by credit balance, not daily quotas. Tenants can send as long as they have sufficient credit.
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

All responses include `X-Request-ID` header for traceability. Error responses follow RFC 7807.

## Webhooks

- Providers (SendGrid, Mailgun, Twilio, FCM, APNS) POST to `/webhooks/{provider}` (to be implemented)
- Verify signatures per provider best practices; store events for compliance/audit

## Events

- **Inbound subjects**: `notifications.events` containing messages from other services (`invoice.due`, `payment.success`)
- **Outbound subjects**: `notifications.delivery.*` with status updates
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
