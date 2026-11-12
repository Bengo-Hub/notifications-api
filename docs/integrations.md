# Notifications Service - Integration Guide

## Overview

The Notifications Service is a centralized communications platform for all Codevertex services. This document details all integration points, protocols, data flows, and implementation guidelines.

---

## Table of Contents

1. [Integration Patterns](#integration-patterns)
2. [Internal Codevertex Service Integrations](#internal-codevertex-service-integrations)
3. [External Provider Integrations](#external-provider-integrations)
4. [Event-Driven Architecture](#event-driven-architecture)
5. [Two-Tier Configuration Management](#two-tier-configuration-management)
6. [Integration Security](#integration-security)
7. [Error Handling & Resilience](#error-handling--resilience)

---

## Integration Patterns

### 1. REST API Pattern (Synchronous)

**Use Case**: Immediate notification delivery

**Endpoint**: `POST /v1/{tenantId}/notifications/messages`

**Request**:
```json
{
  "channel": "email",
  "template": "order_confirmation",
  "recipient": "customer@example.com",
  "data": {
    "order_id": "ORD-123",
    "customer_name": "John Doe"
  },
  "idempotency_key": "unique-key-123"
}
```

**Response**:
```json
{
  "message_id": "msg-uuid",
  "status": "accepted",
  "estimated_delivery": "2024-12-05T10:31:00Z"
}
```

### 2. Event-Driven Pattern (Asynchronous)

**Use Case**: Event-triggered notifications

**Transport**: NATS JetStream

**Flow**:
1. Service publishes event (e.g., `treasury.invoice.due`)
2. Notifications service consumes event
3. Maps event to template and channel
4. Sends notification
5. Publishes delivery status event

### 3. gRPC Pattern (High-Throughput)

**Use Case**: Bulk notifications, high-volume scenarios

**Service**: `NotificationService.Send`

**Advantages**:
- Lower latency
- Binary protocol
- Streaming support

### 4. Webhook Pattern (Callbacks)

**Use Case**: Provider delivery status callbacks

**Endpoints**:
- `/v1/webhooks/sendgrid` - SendGrid delivery callbacks
- `/v1/webhooks/twilio` - Twilio SMS status callbacks
- `/v1/webhooks/fcm` - Firebase Cloud Messaging delivery callbacks

---

## Internal Codevertex Service Integrations

### Auth Service

**Integration Type**: REST API + Events

**Use Cases**:
- User lookup for notification delivery
- Tenant context for notification routing
- OTP delivery for MFA

**REST API Usage**:
- `GET /api/v1/users/{id}` - Get user email/phone for delivery
- `GET /api/v1/tenants/{id}` - Get tenant branding/preferences

**Events Consumed**: None (auth-service publishes user events, but notifications handles OTP via direct API)

**Events Published**: None

**Configuration**:
- Auth-service base URL: `AUTH_SERVICE_BASE_URL` (environment variable)
- JWT validation via `shared/auth-client` library

### Treasury App

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Invoice due reminders
- Payment receipt delivery
- Payment link notifications
- Dunning sequence automation

**Events Consumed**:
- `treasury.invoice.created` - Send invoice notification
- `treasury.invoice.due` - Send due reminder
- `treasury.payment.success` - Send payment receipt
- `treasury.payment.failed` - Send payment failure notification
- `treasury.payment_link.generated` - Send payment link

**Events Published**:
- `notifications.delivery.completed` - Invoice email opened
- `notifications.engagement.tracked` - Payment link clicked

**REST API Usage**:
- `GET /api/v1/invoices/{id}` - Fetch invoice details for attachment
- `GET /api/v1/tenants/{id}/branding` - Get tenant branding for email templates

**Template Mapping**:
- `invoice_created` → `invoice_created_email` template
- `invoice_due` → `invoice_due_reminder` template
- `payment_success` → `payment_receipt` template
- `payment_failed` → `payment_failed_notification` template

### Ordering-Backend

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Order confirmation notifications
- Order status updates
- Delivery ETA notifications
- Loyalty point notifications

**Events Consumed**:
- `cafe.order.created` - Send order confirmation
- `cafe.order.status.changed` - Send status update
- `cafe.order.ready` - Notify customer order ready
- `cafe.delivery.assigned` - Send delivery ETA
- `cafe.loyalty.points_awarded` - Send loyalty notification

**Events Published**: None

**REST API Usage**:
- `GET /api/v1/orders/{id}` - Fetch order details for template

**Delivery details in the order placed email (2026-10-18)**: `ordering.order.created` carries flat `delivery_*` keys for delivered orders (`delivery_address`, `delivery_fee`, `delivery_free`, `delivery_area`, `delivery_method` zone or per_km, `delivery_distance_km`, `delivery_distance_type`, `delivery_eta_minutes`, `delivery_needs_confirmation`). `cmd/worker/order_delivery_details.go` turns them into template values and `templates/email/ordering/order_placed.html` prints a "Delivery details" block; when `delivery_needs_confirmation` is set (a distance-priced fee outside the named areas, tenant policy in logistics) it asks the customer to check the address and fee and to reply before dispatch. Pickup and dine-in orders show no block. WhatsApp is unchanged: the approved `ordering_order_received_v1_btn` template has fixed parameters, so a WhatsApp delivery block needs a new Meta template first.

**Template Mapping**:
- `order_created` → `order_confirmation_email` template
- `order_status_changed` → `order_status_update` template
- `order_ready` → `order_ready_sms` template

### POS Service

**Integration Type**: Events (NATS, `pos.>`). Customer notices go by email and WhatsApp only, through
`sendToParties` (`cmd/worker/pos_consumer.go`, table `posNotices`). Each has an email template and an approved
WhatsApp UTILITY template; a customer with neither contact gets nothing.

| Event | Template | WhatsApp template | Contacts (payload) |
|---|---|---|---|
| `pos.payment.received_notification` | `pos/pos_payment_receipt` | `pos_payment_received_v1` | `customer_email`, `customer_phone` |
| `pos.loyalty.points.earned` | `pos/loyalty_points_earned` | `pos_loyalty_points_v1` | `customer_email`, `customer_phone` |
| `pos.loyalty.tier_upgraded` | `pos/loyalty_tier_upgraded` | `pos_loyalty_tier_v1` | `customer_email`, `customer_phone` |
| `pos.loyalty.referral_earned` | `pos/loyalty_referral_earned` | `pos_loyalty_referral_v1` | `referrer_email`, `referrer_phone` |
| `pos.return.completed` | `pos/return_completed` | `pos_refund_processed_v1` | `customer_email`, `customer_phone` |
| `pos.layaway.payment_due` | `pos/layaway_payment_due` | `pos_layaway_due_v1` | `customer_email`, `customer_phone` |
| `pos.hotel.check_in` | `pos/hotel_check_in` | `pos_hotel_check_in_v1` | `guest_email`, `guest_phone` |
| `pos.hotel.check_out` | `pos/hotel_check_out` | `pos_hotel_check_out_v1` | `guest_email`, `guest_phone` |

**Recipient from the CRM.** The CRM owns customer identity, so pos-api and library-api send only `crm_contact_id` (plus
record ids) for a person linked to the CRM, and no name, email or phone. The POS and library consumers fetch the
contact's name, email and phone from the CRM at send time (`internal/contacts`, shared CRM client `BatchGet`, answers
cached for 5 minutes per tenant and contact). `crm_contact_id` always names the person the notice goes to; for
`pos.loyalty.referral_earned` that is the referrer, and the CRM details fill the `referrer_*` fields. For hotel check-in
and check-out the CRM details are used for the notice only; the guest register in pos-api is unchanged. A person with no
CRM link still arrives with the payload fields in the table above (and `email`/`name` for library events), and those are
used as they are. When both are present the CRM wins field by field.

When an event names a CRM contact and has no email or phone of its own:
- If the CRM cannot be reached, the message is redelivered after 30 seconds, up to 3 deliveries in all
  (`cmd/worker/crm_retry.go`). On the last delivery it is acknowledged and dropped with one "gave up after max
  deliveries" log line.
- If the CRM answers but has no email or phone for the contact, the CRM client is switched off (no URL or key), or the
  CRM does not know the contact (erased, or held under another tenant id), the notice is dropped with one log line.
- A linked library member's notice never goes to the librarian instead. The librarian copy is only for members with no
  CRM link and no email, as before.

The 3-delivery bound is the durable's `MaxDeliver` (`crmRetryMaxDeliver`). nats.go will not bind to an existing durable
whose `MaxDeliver` differs, so a change to that value also needs `notifications-pos-orders` (pos stream) and
`notifications-library` (library stream) edited or deleted once on the server. A contact deleted or erased in the CRM
can still be served from the cache for up to 5 minutes. Logs carry tenant and contact ids only.

Sale receipts are handled by `internal/modules/events` (`handleSaleNotification`):
- `pos.sale.notification_requested` (the cashier's Send Notification): email or WhatsApp as pos-api resolved it. For a
  customer linked to the CRM the event carries `crm_contact_id` and no name; the API process looks the contact up in the
  CRM (`sale_recipient.go`, the same `internal/contacts` resolver, wired in `internal/app`). A `customer_phone` or
  `customer_email` on a linked event is what the cashier typed for this send and wins over the CRM; the name always
  comes from the CRM. With no email or phone left: the CRM unreachable means a redelivery after 30 seconds until the
  durable's last delivery (`notif-pos-sale-notify`, `MaxDeliver` 5), then one "gave up" log line and a drop; a CRM
  answer with no email or phone, an unknown contact or a switched off CRM client means a drop with one log line. Logs
  carry tenant and contact ids only. An unlinked customer's fields are used as they are.
- `pos.sale.finalized` (automatic, every finished sale): email when the sale has an address; WhatsApp only when the
  tenant switched on "WhatsApp receipt after every sale" (`pos/receipt_on_sale`, optional, off by default). This one
  still reads the sale's own `customer_phone` and never asks the CRM, so the automatic receipt costs no CRM call per
  sale; if pos-api ever drops the phone from `sale.finalized`, add the same lookup here.

Removed on 2026-10-31 because they could never deliver: order status changed and payment recorded (no customer
contact, they fell back to emailing the business), waiter called (no phone; the kitchen screen handles it live),
appointment created and reminder (nothing publishes them), POS low stock (duplicate of the inventory low stock email to
the tenant admin). The duplicate loyalty and waiter handlers in `internal/modules/events` were removed too.

### Logistics Service

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Delivery ETA updates
- Delivery completion notifications
- Driver assignment notifications

**Events Consumed**:
- `logistics.task.assigned` - Notify customer driver assigned
- `logistics.task.en_route` - Send ETA update
- `logistics.task.completed` - Send delivery confirmation

**Events Published**: None

**Template Mapping**:
- `task_assigned` → `driver_assigned_sms` template
- `task_en_route` → `eta_update_push` template
- `task_completed` → `delivery_complete_email` template

**Vehicle insurance (events)**: `logistics.fleet.vehicle_insurance_expiring` and
`logistics.fleet.vehicle_insurance_expired` (logistics-api's hourly insurance monitor; payload: plate,
type, policy, insurer, `expires_on` YYYY-MM-DD, `days_left`, `blocked`, rider name, email and phone).
`cmd/worker/vehicle_insurance.go` sends each through `sendToParties` to two parties: the rider on
the vehicle (button to `<slug>/profile` on the rider app) and the business contact (button to
`<slug>/vehicles` on the logistics app), by email (`logistics/vehicle_insurance_expiring|expired`,
worded per `audience`) and WhatsApp (`logistics_insurance_{expiring,expired}_{rider,business}_v1_btn`).
The WhatsApp templates need the template sync and Meta approval before they send.

**Direct send (REST)**: `logistics/rider_account` (email). logistics-api posts it to
`POST /api/v1/{tenant}/notifications/messages` with `Idempotency-Key rider-account:<member id>` when an
admin onboards an internal rider. Data: `name`, `email`, `temp_password` (empty for a rider who
already had a platform account, which hides the password row), `login_url` (rider app). It is sent
directly, not through the logistics outbox, so the temporary password never lands in an event table;
it rides only in this service's NATS queue until retention drops it, the same as
`digitika/portal_account`. Email only, like the portal-account precedent: a password in a WhatsApp
template would also need Meta approval and is not sent there. Rider lifecycle mails for freelance
riders still come from `logistics.fleet.*` events (`cmd/worker/fleet_consumer.go`).

### Projects Service

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Task assignment notifications
- Deadline reminders
- Comment mentions
- Meeting reminders

**Events Consumed**:
- `projects.task.assigned` - Notify team member
- `projects.task.due` - Send deadline reminder
- `projects.comment.created` - Notify mentioned users
- `projects.meeting.scheduled` - Send meeting invitation

**Events Published**: None

**Template Mapping**:
- `task_assigned` → `task_assignment_email` template
- `task_due` → `deadline_reminder_push` template
- `comment_mention` → `comment_mention_email` template

### ERP Service

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Payroll notifications (payslips, runs, disbursement, consultant vouchers)
- HRM notifications (leave decisions, appraisals)
- Recruitment notifications (application, interview, offer, welcome)
- Onboarding notifications (task assignment, completion)
- Expense approval notifications

**Events Consumed**:
- `erp.payroll.generated` - Send payslip notification
- `erp.leave.approved` - Send leave approval
- `erp.expense.approved` - Send expense approval
- `erp.email.requested` - Render a named template (referenced via `template_id`)

**Events Published**: None

**Template Mapping** (email `template_id` referenced by `erp.email.requested`):

Payroll:
- `payroll/payslip_email` - Employee payslip (PDF payslip attached)
- `payroll/payroll_processed` - Manager notice that a payroll run was processed
- `payroll/payroll_disbursed` - Notice that a payroll run was disbursed
- `payroll/consultant_voucher_email` - Consultant payment voucher (PDF voucher attached)

HRM:
- `hrm/leave_approved` - Leave request approved
- `hrm/leave_rejected` - Leave request declined
- `hrm/leave_pending` - Leave request submitted / pending approval
- `hrm/appraisal_due` - Performance appraisal due

Recruitment:
- `recruitment/application_received` - Application acknowledged
- `recruitment/interview_scheduled` - Interview invitation
- `recruitment/offer_extended` - Job offer (offer details/PDF attached)
- `recruitment/employee_welcome` - New-hire welcome

Onboarding:
- `onboarding/onboarding_task_assigned` - Onboarding task assigned
- `onboarding/onboarding_completed` - Onboarding completed

### TruLoad Backend

**Integration Type**: Events (NATS) + REST API

**Use Cases**:
- Officer notifications
- Station alerts
- System notifications

**Events Consumed**:
- `truload.weighing.completed` - Send weighing summary
- `truload.case.created` - Notify case manager
- `truload.system.alert` - Send system alerts

**Events Published**: None

**Note**: TruLoad integrates only with auth-service and notifications-service. No treasury integration.

---

## External Provider Integrations

### Email Providers

#### SendGrid (Primary)

**Configuration** (Tier 1 - Developer Only):
- API Key: Stored encrypted at rest in database
- From Email: Configured per tenant (Tier 2)
- From Name: Configured per tenant (Tier 2)

**Integration**:
- REST API: `https://api.sendgrid.com/v3/mail/send`
- Authentication: Bearer token (API key)
- Webhook: Delivery status callbacks

**Features**:
- Template support
- Attachment support
- Bounce handling
- Open/click tracking

#### Mailgun (Fallback)

**Configuration** (Tier 1):
- API Key: Stored encrypted at rest
- Domain: Configured per tenant (Tier 2)

**Integration**:
- REST API: `https://api.mailgun.net/v3/{domain}/messages`
- Authentication: Basic auth (API key)

**Use Case**: Failover when SendGrid unavailable

#### SMTP (Fallback)

**Configuration** (Tier 1):
- SMTP Host: Stored encrypted
- SMTP Port: Stored encrypted
- Username: Stored encrypted
- Password: Stored encrypted

**Use Case**: Custom SMTP server fallback

### SMS Providers

#### Twilio (Primary)

**Configuration** (Tier 1):
- Account SID: Stored encrypted at rest
- Auth Token: Stored encrypted at rest
- Phone Number: Configured per tenant (Tier 2)

**Integration**:
- REST API: `https://api.twilio.com/2010-04-01/Accounts/{AccountSid}/Messages.json`
- Authentication: Basic auth (AccountSid:AuthToken)
- Webhook: Delivery status callbacks

**Features**:
- Global coverage
- Delivery receipts
- Status callbacks

#### Africa's Talking (Regional)

**Configuration** (Tier 1):
- API Key: Stored encrypted at rest
- Username: Stored encrypted
- Short Code: Configured per tenant (Tier 2)

**Integration**:
- REST API: `https://api.africastalking.com/version1/messaging`
- Authentication: API key header

**Use Case**: Kenya-specific SMS delivery

#### Custom SMS Provider

**Configuration** (Tier 1):
- Provider URL: Stored encrypted
- API Key: Stored encrypted
- Custom headers: Stored encrypted

**Use Case**: Tenant-specific SMS provider

### Push Notification Providers

#### Firebase Cloud Messaging (FCM)

**Configuration** (Tier 1):
- Server Key: Stored encrypted at rest
- Project ID: Stored encrypted

**Integration**:
- REST API: `https://fcm.googleapis.com/v1/projects/{project_id}/messages:send`
- Authentication: OAuth 2.0 access token

**Features**:
- Android push notifications
- iOS push notifications (via APNs)
- Web push notifications
- Topic subscriptions

#### Apple Push Notification Service (APNs)

**Configuration** (Tier 1):
- Key ID: Stored encrypted
- Team ID: Stored encrypted
- Bundle ID: Configured per tenant (Tier 2)
- Private Key: Stored encrypted

**Integration**:
- HTTP/2 API: `https://api.push.apple.com/3/device/{token}`
- Authentication: JWT (signed with private key)

**Use Case**: Direct iOS push notifications

### WhatsApp Business API (Future)

**Configuration** (Tier 1):
- Business Account ID: Stored encrypted
- Access Token: Stored encrypted
- Phone Number ID: Stored encrypted

**Status**: Planned for future implementation

#### Tenant template sync (own WhatsApp Business Account)

Meta approves message templates per WhatsApp Business Account (WABA). A tenant that connects its own number through Embedded Signup gets a new WABA with none of our templates, so every template send from its number fails until they exist there. Hadia hit exactly this (WABA 1625290866305102), while Urban Loft worked because its WABA already had them.

I sync only the templates a tenant can actually send (owner decision "Only what the tenant uses"), and I do it automatically after a successful Embedded Signup plus on demand from a button (owner decision "Auto-sync on connect + button").

- Code: `internal/whatsapp/tenanttemplates` (selection, usage sources, sync and status), reusing `internal/whatsapp/templatesync` against the tenant's WABA and token from `Manager.LoadTenantWhatsAppTemplateCredentials`. That loader reads only the tenant's own whatsapp/meta_cloud row (`waba_id`, encrypted `access_token`) and never falls back to the platform WABA.
- Routes (tenant admins, the billing-manage permission that connects the number; the tenant comes from the token, X-Tenant-ID only for a platform owner):
  - `GET /api/v1/settings/whatsapp/templates`: each selected template with its Meta status (APPROVED, PENDING, REJECTED with Meta's reason, or MISSING).
  - `POST /api/v1/settings/whatsapp/templates/sync` with `{"dry_run": false}`: submits up to 10 missing templates per call and returns `remaining`; the UI calls again until it is 0.
- Auto-sync: Embedded Signup starts `Service.AutoSync` in the background once the number is saved. It runs batch after batch on a detached context (10 minute limit), logs the outcome and never changes the signup response.

**Which templates a tenant gets.** The map lives in `internal/whatsapp/tenanttemplates/selection.json`. Setting `WHATSAPP_TENANT_TEMPLATE_MAP_FILE` to a JSON file of the same shape replaces it without a release.

| Service or product | Manifest prefixes |
|---|---|
| always (shared) | `auth_`, `subscription_`, `credits_`, `broadcast_`, `occasion_` |
| ordering | `ordering_` |
| pos | `pos_` |
| logistics | `logistics_` |
| inventory | `inventory_` |
| treasury, finance | `finance_` |
| erp | `finance_`, `hrm_`, `payroll_` |
| hrm / payroll | `hrm_` / `payroll_` |
| isp, ispbilling | `ispbilling_` |
| library | `library_` |
| maskani | `maskani_` |
| digitika | `digitika_` |
| hadia | `hadia_` |

A tenant uses a service when either source names it:

1. The caller's token: `active_service_tags` and `active_products`, read only when the token belongs to that tenant (a platform owner acting for a tenant carries the platform's own entitlements).
2. The tenant's delivery logs from the last 180 days, any channel and outcome: the first segment of the template id, so `ordering/order_placed` means ordering and `hadia/gift_receipt` means hadia. A WhatsApp send skipped for want of a template still counts.

Names in neither list are ignored (`whatsapp/inbox_reply` is a free reply inside the 24 hour window and needs no template). Platform-only templates (`codevertex_` enquiries, `provider_balance_low`) appear in no list, so they never reach a tenant's WABA.

#### Business profile and display name (tenant numbers)

On 2026-11-12 Hadia's newly connected number (phone number ID 1446431481882844, WABA 1625290866305102) showed customers a bare number: no name, no photo. Two separate things in Meta decide how a Cloud API number looks, and I manage both per tenant.

**1. The display name (`verified_name`).** This is the name given at signup, and Meta reviews it. What Meta's documentation says, checked on 2026-11-12:

- The display name always appears in the number's WhatsApp profile. It appears at the top of individual chats and in the chat list only once the name is approved through display name verification ([Display names](https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names)).
- That verification is not something I can request: it runs on its own when the business reaches a higher messaging limit ([Display names](https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names)). A new business portfolio starts at a messaging limit of 250; it reaches 2,000 by completing Meta Business Verification, through partner verification, or by sending 2,000 delivered messages outside customer service windows to unique numbers within 30 days with high quality templates ([Messaging limits](https://developers.facebook.com/docs/whatsapp/messaging-limits)).
- Until then, for a business that has not completed Business Verification, the name shows only in small text next to the phone number in the contacts view, not in the chat list or the chat itself; after verification it shows in both ([AWS End User Messaging, WhatsApp business account types](https://docs.aws.amazon.com/social-messaging/latest/userguide/whatsapp-business-account.html), which restates Meta's rule; Meta's own page above gives the same condition in shorter form).
- A customer who saved the number under their own name sees that name instead ([Display names](https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names)).
- An Official Business Account adds a checkmark in the profile and chat header. It needs a verified business portfolio, two step verification on the number and a notable, often searched brand; Meta decides at its discretion ([Official Business Accounts](https://developers.facebook.com/documentation/business-messaging/whatsapp/official-business-accounts/)).
- `name_status` values: `APPROVED`, `AVAILABLE_WITHOUT_REVIEW`, `PENDING_REVIEW`, `DECLINED`, `EXPIRED`, `NONE` ([Phone numbers](https://developers.facebook.com/docs/whatsapp/cloud-api/phone-numbers)). A requested change shows as `new_display_name` and `new_name_status`. A name can be changed 10 times in 30 days; once a new name is approved the number must be registered again (`POST /{phone-number-id}/register`) within 14 days, and registering before approval does nothing ([Display names](https://developers.facebook.com/documentation/business-messaging/whatsapp/display-names)).

The phone number endpoint gives no reason for a declined name; Meta sends it only in the `phone_number_name_update` webhook, which I do not store yet, so the settings page explains the usual cause (the name must match the registered business or brand) instead.

So for Hadia the name will only show in chats once Hadia's business portfolio is verified and its limit rises. The photo and profile below show right away.

**2. The business profile.** `GET` and `POST /{phone-number-id}/whatsapp_business_profile` ([Business profiles](https://developers.facebook.com/docs/whatsapp/cloud-api/reference/business-profiles)). Fields and Meta's limits, all enforced before calling Meta:

| Field | Limit |
|---|---|
| `about` | 1 to 139 characters, cannot be empty |
| `address` | 256 characters |
| `description` | 512 characters |
| `email` | 128 characters, valid email |
| `websites` | at most 2, each at most 256 characters, with `http://` or `https://` (I add `https://` when missing) |
| `vertical` | one of Meta's 21 categories (`RESTAURANT`, `RETAIL`, `BEAUTY` ...) or empty |
| `profile_picture_handle` | a handle from the Resumable Upload API |

POST needs `"messaging_product": "whatsapp"`. The GET answer is wrapped in `data[0]` and carries `profile_picture_url` once a photo is set.

**The photo.** Meta's [Resumable Upload API](https://developers.facebook.com/docs/graph-api/guides/upload): `POST /{app-id}/uploads?file_name&file_length&file_type` opens a session (`{"id":"upload:..."}`, sometimes with its own `?sig=` query, which I keep as given), then `POST /upload:{session}` with the raw bytes, header `file_offset: 0` and `Authorization: OAuth <token>` (OAuth, not Bearer) returns `{"h": "<handle>"}`, which I set as `profile_picture_handle`. The app ID is the platform's `app_id` on Platform > Providers > WhatsApp (`WHATSAPP_APP_ID` as fallback); the token is the tenant's own business token from Embedded Signup. Upload file types are PDF, JPEG, PNG and MP4; I accept JPEG and PNG only, sniffed from the bytes. The current Cloud API reference names no size or dimension limits, so I apply Meta's published profile photo rules: at most 5 MB, at least 192x192 (640x640 is best; WhatsApp crops to a circle, so the logo should sit in the middle).

**Code and routes.**

- Service: `internal/whatsapp/profile` (`Get`, `Update`, `SetPhotoBytes`, `SetPhotoFromURL`, `SyncFromBranding`, `AutoSync`). Credentials come from `Manager.LoadTenantWhatsAppProfileCredentials`: the tenant's own whatsapp/meta_cloud row only (`phone_number_id`, encrypted `access_token`, optional `api_version`), never the platform's number. The Graph version is `whatsapp.GraphVersion`.
- Branding comes from auth-api the same way the worker's email templates get it (`profile.AuthAPIBranding`): the local tenant row gives the slug, then the shared cache library's `GetTenantDetails` gives name, `logo_url`, `contact_email`, `website`, and metadata `tagline` (used as `about`), `description` and `address` when present.
- Routes, tenant admins only (billing-manage, the same guard as the template routes; the tenant comes from the token, X-Tenant-ID only for a platform owner). Meta calls run on a detached context with a 2 minute limit:
  - `GET /api/v1/settings/whatsapp/profile`: display name, its status and any pending new name, quality rating, the profile fields and photo URL.
  - `PUT /api/v1/settings/whatsapp/profile`: any of `about`, `address`, `description`, `email`, `websites`, `vertical`; answers with the profile as Meta now has it.
  - `POST /api/v1/settings/whatsapp/profile/photo`: multipart field `file`, or `{"url": "..."}` (http, https or a `data:` URI). A URL is only fetched from a public address: loopback, private, link local and carrier grade NAT ranges are refused, redirects included.
  - `POST /api/v1/settings/whatsapp/profile/sync-branding`: fills only the empty fields, and the photo when none is set, from the branding. Returns `filled`, `warnings` (an SVG logo, an invalid email) and the profile.
- Errors: no connected number or a value Meta would refuse is a 400 with a plain reason; Meta's own refusal is a 502 carrying Meta's message, code and `fbtrace_id`.
- After Embedded Signup: `WithProfileAutoSync(Service.AutoSync)` runs `SyncFromBranding` in the background next to the template auto-sync, after a successful save only, with a 3 minute limit. It only logs; it never changes the signup answer.

To fix Hadia's number now: open Settings > Providers > WhatsApp as a Hadia admin and press "Use our logo and details", or call `POST /api/v1/settings/whatsapp/profile/sync-branding` as a platform owner with `X-Tenant-ID` set to Hadia's tenant ID.

---

## Event-Driven Architecture

### Inbound Events (Consumed)

**Event Format**:
```json
{
  "event_id": "uuid",
  "event_type": "treasury.invoice.due",
  "tenant_id": "tenant-uuid",
  "timestamp": "2024-12-05T10:30:00Z",
  "data": {
    "invoice_id": "inv-uuid",
    "customer_email": "customer@example.com",
    "amount": 1000.00,
    "due_date": "2024-12-10"
  }
}
```

**Event Processing Flow**:
1. Consume event from NATS JetStream
2. Map event type to template and channel
3. Fetch recipient data (email/phone) from auth-service or event payload
4. Render template with event data
5. Select provider based on tenant configuration
6. Send notification via provider
7. Log delivery attempt
8. Publish delivery status event

### Outbound Events (Published)

**notifications.delivery.accepted**
```json
{
  "event_id": "uuid",
  "event_type": "notifications.delivery.accepted",
  "tenant_id": "tenant-uuid",
  "timestamp": "2024-12-05T10:30:00Z",
  "data": {
    "message_id": "msg-uuid",
    "channel": "email",
    "recipient": "customer@example.com",
    "provider": "sendgrid",
    "provider_message_id": "sg-msg-id"
  }
}
```

**notifications.delivery.completed**
```json
{
  "event_id": "uuid",
  "event_type": "notifications.delivery.completed",
  "tenant_id": "tenant-uuid",
  "timestamp": "2024-12-05T10:30:00Z",
  "data": {
    "message_id": "msg-uuid",
    "channel": "email",
    "recipient": "customer@example.com",
    "provider": "sendgrid",
    "delivered_at": "2024-12-05T10:30:05Z",
    "opened_at": "2024-12-05T10:35:00Z"
  }
}
```

**notifications.delivery.failed**
```json
{
  "event_id": "uuid",
  "event_type": "notifications.delivery.failed",
  "tenant_id": "tenant-uuid",
  "timestamp": "2024-12-05T10:30:00Z",
  "data": {
    "message_id": "msg-uuid",
    "channel": "email",
    "recipient": "customer@example.com",
    "provider": "sendgrid",
    "error_code": "bounce",
    "error_message": "Invalid email address",
    "retry_count": 3
  }
}
```

---

## Two-Tier Configuration Management

### Tier 1: Developer/Superuser Configuration

**Visibility**: Only developers and superusers

**Configuration Items**:
- Provider API keys and secrets
- SMTP credentials
- Webhook signing secrets
- Encryption keys
- Database credentials

**Storage**:
- Encrypted at rest in database
- K8s secrets for runtime
- Vault for production secrets

**Management**:
- Admin API endpoints (superuser only)
- Encrypted storage with AES-256-GCM
- Key rotation every 90 days

### Tier 2: Business User Configuration

**Visibility**: Normal system users (tenant admins)

**Configuration Items**:
- From email address
- From name
- SMS short code
- Template branding (colors, logos)
- Notification preferences
- Channel enable/disable
- Rate limits

**Storage**:
- Plain text in database (non-sensitive)
- Tenant-specific configuration tables

**Management**:
- Self-service API endpoints
- Tenant admin UI
- Tenant-scoped access control

---

## Integration Security

### Authentication

**JWT Tokens**:
- Validated via `shared/auth-client` library
- JWKS from auth-service
- Token claims include tenant_id for scoping

**API Keys** (Service-to-Service):
- Stored in K8s secrets
- Rotated quarterly
- Per-tenant API keys for external integrations

### Authorization

**Tenant Isolation**:
- All requests scoped by tenant_id
- Provider credentials isolated per tenant
- Templates isolated per tenant

**RBAC**:
- Service-level roles (admin, operator, viewer)
- Tenant admin roles (manage templates, preferences)
- Fine-grained permissions per operation

### Secrets Management

**Encryption**:
- Secrets encrypted at rest (AES-256-GCM)
- Decrypted only when used
- Key rotation every 90 days

**Access Control**:
- Tier 1 secrets: Superuser only
- Tier 2 configuration: Tenant admin access
- Audit logging for all secret access

### Webhook Security

**Signature Verification**:
- HMAC-SHA256 signatures
- Secret shared via K8s secret
- Timestamp validation (5-minute window)
- Nonce validation (prevent replay attacks)

---

## Error Handling & Resilience

### Retry Policies

**Exponential Backoff**:
- Initial delay: 1 second
- Max delay: 30 seconds
- Max retries: 3

**Provider Failover**:
- Primary provider failure → Fallback provider
- Health checks every 30 seconds
- Automatic failover on consecutive failures

### Circuit Breaker

**Configuration**:
- Opens after 5 consecutive failures
- Half-open after 60 seconds
- Closes on successful request

**Use Case**: Prevent cascading failures when provider is down

### Dead Letter Queue

**Configuration**:
- Failed events after max retries → DLQ
- Manual reconciliation interface
- Alert on DLQ size threshold

### Monitoring

**Metrics**:
- Delivery success/failure rates
- Provider latency (p50, p95, p99)
- Event processing latency
- Template rendering latency

**Alerts**:
- High failure rate (>5%)
- Provider unavailability
- DLQ size threshold
- Rate limit exceeded

---

## References

- [SendGrid API Documentation](https://docs.sendgrid.com/)
- [Twilio API Documentation](https://www.twilio.com/docs)
- [Firebase Cloud Messaging Documentation](https://firebase.google.com/docs/cloud-messaging)
- [NATS JetStream Documentation](https://docs.nats.io/nats-concepts/jetstream)

