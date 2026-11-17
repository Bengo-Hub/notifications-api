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
- `treasury.escrow.reconciliation_shortfall` and `treasury.escrow.reconciliation_recovered` - escrow
  wallet shortfall and recovery, platform ops alerts (see below)

**Escrow reconciliation alerts (added 13 November 2026)**: treasury's hourly escrow reconciliation
(`internal/modules/escrow/reconcile.go`, `ReconcileAll`) publishes
`treasury.escrow.reconciliation_shortfall` once when a tenant's PayHero wallet first holds less than
its pots and releases in flight owe, and `treasury.escrow.reconciliation_recovered` once when it
covers them again. While the incident is open treasury refuses Team wallet withdrawals and skips the
daily commission sweep. Consumer `internal/modules/events/escrow_shortfall_alert.go` on stream
`treasury`, durables `notif-escrow-reconciliation-shortfall` and
`notif-escrow-reconciliation-recovered`, both new messages only so a first rollout does not replay
the 72 hours the stream keeps. Both take the ops alert path (`sendOpsAlert` in
`gateway_health_alert.go`): the shortfall goes by email to `PLATFORM_ALERT_EMAILS`
(`platform/escrow_shortfall`), the Slack webhook and the ops WhatsApp numbers in auth-api's
`platform_alert_channels`; the recovery (`platform/escrow_recovered`) by email and Slack only. Both
templates are locked. The email names the tenant(s) by name or slug from the local tenant table
(filled from auth-api by id), never personal data, with the scope (own account, or the shared
account and its PayHero vendor id), the amount short, what is blocked and what to do (top up or
investigate the wallet; the reconciliation recovers by itself).

Tenants on one PayHero root wallet (`scope: shared_account`, with `vendor_id` and `shared_owed`)
each get their own incident and event; the consumer gathers a vendor's events for 10 seconds and
sends one alert listing every tenant with its pots. Dedupe keeps one alert per incident, not one per
hourly check or redelivery: the key is the `incident_id` (tenant plus day when it is missing) or,
for a shared account, the vendor id, and it holds the last state alerted (short or recovered) for 14
days in Redis (in the process when Redis is down), so only a change of state sends.

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

**Events Consumed** (`cmd/worker/delivery_consumer.go`, customer emails):

| Event | Email template |
|-------|----------------|
| `logistics.task.assigned` | `logistics/delivery_assigned` (the rider also gets `logistics/rider_job_assigned`) |
| `logistics.task.en_route_dropoff`, `logistics.task.en_route` | `logistics/delivery_en_route` |
| `logistics.task.completed`, `logistics.task.delivered` | `logistics/delivery_completed` |
| `logistics.task.failed` | `logistics/delivery_failed` |

The customer is the CRM contact when the event carries `crm_contact_id` (looked up through
`internal/contacts`, retried while the CRM is down, dropped when the CRM has no email), else the
event's `customer_email` and `customer_name`. An event with neither sends nothing. Tasks with
`source_service` `ordering` send none of these: ordering-backend owns the customer conversation
for its own orders. The emails read `order_number` (else `tracking_code`), `rider_name`,
`rider_phone`, `estimated_arrival`, `delivered_at`, `delivered_via`, `recipient_name`,
`proof_photo` and `failure_reason`; the Track button opens the logistics public tracker
(`/track/{tracking_code}`) and the failed email links the tenant's contact email, else shows its contact phone.

**Events Published**: None

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
- `project.milestone.reached` - `projects/project_milestone_reached` to the tenant's contact email
- `project.task.mentioned`, `project.task.assigned` - task notices to one teammate (see below)
- `projects.task.due` - Send deadline reminder
- `projects.meeting.scheduled` - Send meeting invitation
- `project.qa.invite.created`, `project.qa.result.failed`, `project.qa.session.closed`,
  `project.qa.session.accepted`, `project.qa.session.rejected` - QA acceptance (see below)

**QA acceptance (added 13 November 2026)**: projects-api's QA module (`internal/services/qa`,
`PublishQAEvent`) publishes `project.qa.<type>` through its outbox with aggregate `project`, so
the events ride the existing `projects` stream (`project.>`). The worker's `notifications-projects`
durable sees them and acks them as unmapped; they are sent by the events subscriber
(`internal/modules/events/project_qa.go`), durable `notifications-projects-qa` on `project.qa.>`,
because the ops alert path (`sendOpsAlert`) lives there. Every payload carries `scope`: `platform`
for a Codevertex service session and `tenant` otherwise. A tenant event (or one with no scope)
never reaches ops.

| Event | Platform scope | Tenant scope |
|---|---|---|
| `invite.created` | `qa/invite` to the invited address, sent from Codevertex | `qa/invite` to the invited address, tenant sender and branding |
| `result.failed` | Ops: `platform/qa_result_failed` email, Slack; WhatsApp for severity 1 only | `qa/result_failed` to the session owner and the tenant's contact (auth-api), admins as fallback |
| `session.closed` | `qa/session_closed` to owner and signers (Codevertex); ops email and Slack (`platform/qa_session_update`) | `qa/session_closed` to owner and signers; tenant contact when neither is reachable |
| `session.accepted`, `session.rejected` | `qa/session_decided` to owner and signers; ops email and Slack | `qa/session_decided` to owner and signers; tenant contact as fallback |

The owner is read from the local user table by `owner_id` (auth user id). Signers come from an
optional `signers` list (`[{email, name}]`) that projects-api does not send yet (it keeps only
address hashes), so until then the owner gets the close and decision notices. The invite carries
the address in the event because projects-api does not store it; this service puts it only in the
email, never in a log. `qa/invite`, `qa/result_failed`, `qa/session_closed` and
`qa/session_decided` are essential; the two `platform/qa_*` alerts are locked. Each event id is
claimed once in Redis (7 days; in the process without Redis) before anything is sent, and every
email carries the idempotency key `project-qa-<event id>`.

**Task mentions and assignments (sprint 12)**: projects-api publishes `project.task.mentioned` and
`project.task.assigned` through its shared-events outbox on the `projects` stream, one event per
recipient and never to the author. The worker's `notifications-projects` durable
(`cmd/worker/projects_consumer.go`) sends each through `sendToParties`:

| Event | Email | WhatsApp |
|---|---|---|
| `task.mentioned` | `projects/task_mentioned` with the comment excerpt | `projects_task_mentioned_v1_btn`, or `projects_task_mentioned_short_v1_btn` when there is no excerpt |
| `task.assigned` | `projects/task_assigned` | `projects_task_assigned_v1_btn` |

Payload: `project_id`, `project_name`, `task_id`, `task_title`, `actor_name`, `recipient_user_id`,
`recipient_email`, `recipient_name`, `comment_excerpt` (mentions only, plain text up to 200
characters) and `action_path` (for example `/{orgSlug}/projects/{id}/tasks?task={taskId}`). The
email goes to `recipient_email`, else the email of the local user for `recipient_user_id`. The
phone comes from the local user table (synced from auth-api) by `recipient_user_id`; with no
phone only the email goes. Links open `action_path` on the projects app: the tenant's `projects`
service URL from auth-api, else `NOTIFICATIONS_PROJECTS_APP_URL`, else
`https://projects.codevertexafrica.com`. The WhatsApp button is fixed to
`https://projects.codevertexafrica.com/{{1}}` with the path as its suffix. Both types are essential
in the preferences registry (group Support & Projects): they are on by default, and a tenant can
mute either one or its WhatsApp channel in settings. Idempotency keys are
`projects-<event type>-<event id>-<channel>`, so a redelivered event is not sent twice.

**Client portal (sprint 14, 2026-11-17)**: the same durable sends the four portal notices through
`sendToParties` (`cmd/worker/projects_portal.go`), one recipient per event:

| Event | To | Email | WhatsApp |
|---|---|---|---|
| `client.invited` | the firm's client (target customer), by the payload's `recipient_email` and `recipient_phone` (read from the CRM by projects-api; the person has no account yet) | `projects/client_invited` | `projects_client_invited_v1_btn` (name, organisation, project) |
| `milestone.ready_for_review` | one client (customer): a signed-in one by `recipient_user_id`, or one still at the invite stage by the payload's `recipient_email` and `recipient_phone` (read from the CRM by projects-api at send time, `recipient_user_id` empty) | `projects/milestone_ready_for_review` | `projects_milestone_review_v1_btn` (name, milestone, project) |
| `milestone.approved` | the project owner or an owner or manager member (staff) | `projects/milestone_approved` | `projects_milestone_approved_v1_btn` with the comment, `projects_milestone_approved_short_v1_btn` without |
| `milestone.changes_requested` | as for approved | `projects/milestone_changes_requested` | `projects_milestone_changes_v1_btn` (with the comment) |

The organisation is the tenant's name from auth-api. Email and phone fall back to the local user
table by `recipient_user_id` as for the task notices. Buttons open `action_path` on the projects
app: `/{org}/portal/projects/{id}` (invite), `?milestone={id}` added for a review, and
`/{org}/projects/{id}/milestones?milestone={id}` for the team. All four are essential (Support &
Projects). The five new WhatsApp templates wait for the Meta sync (Platform WhatsApp templates in
notifications-ui, `POST /platform/whatsapp/templates/sync`); until Meta approves them only the
emails go. The `projects_` prefix in `tenanttemplates/selection.json` already covers them.

**Client thread, change requests and automations (2026-11-17)**: the same durable sends five more
notices by email and WhatsApp (`cmd/worker/projects_notices.go`). They started email only; the
WhatsApp templates came on 2026-11-17 and wait for the Meta sync like the portal ones. Until Meta
approves a template on the sending WABA its send fails and only the email lands.

| Event | To | Email | WhatsApp (UTILITY, button on the projects app) |
|---|---|---|---|
| `client_thread.posted` | the project owner or an owner or manager member (staff), never a client | `projects/client_thread_posted` with the post as `comment_excerpt` | `projects_client_thread_posted_v1_btn` (name, client, project), "Open Thread" |
| `client_thread.replied` | one signed-in client (customer) | `projects/client_thread_replied`, button the portal project's Messages tab | `projects_client_thread_replied_v1_btn` (name, organisation, project), "Read Reply" |
| `change_request.review_requested` | an approver candidate holding change approvals (staff), never the requester | `projects/change_request_review_requested` with the title, kind and description excerpt | `projects_change_request_review_v1_btn` (name, requester, title, project), "Review Request" |
| `change_request.decided` | the requester (staff) | `projects/change_request_decided` with the decision and the decider's comment | `projects_change_request_decided_v1_btn` (name, title, project, `approved` or `rejected`), "Open Request" |
| `task.automation_notice` | every id in `recipient_ids` found in the local user table with a sendable email or a phone; unknown ids are skipped | `projects/task_automation_notice` with the rule, the new status and the rule's message | `projects_task_automation_v1_btn` (name, task, project, new status), "Open Task" |

The WhatsApp bodies carry facts only. The post, the reply, the comment and the rule's message stay
in the email, because a body that is mostly free text is billed as marketing.

The first four have one recipient per event and read it as the task notices do: the email at
`recipient_email`, else the local user's, and the phone at `recipient_phone`, else the local
user's; with no phone only the email goes. The automation notice is one event per rule run: the
ids are looked up in one query (the row of the event's tenant first) and each recipient is its own
party, so the idempotency keys are `projects-task.automation_notice-<event id>-email-<user id>`
and `...-whatsapp-<user id>`, per event, channel and recipient. All five are essential (Support &
Projects); see the registry for why the automation notice is not optional.

**Events Published**: None

**Template Mapping**:
- `task.mentioned` → `projects/task_mentioned` (email, WhatsApp)
- `task.assigned` → `projects/task_assigned` (email, WhatsApp)
- `client.invited` → `projects/client_invited` (email, WhatsApp)
- `milestone.ready_for_review` → `projects/milestone_ready_for_review` (email, WhatsApp)
- `milestone.approved` → `projects/milestone_approved` (email, WhatsApp)
- `milestone.changes_requested` → `projects/milestone_changes_requested` (email, WhatsApp)
- `client_thread.posted` → `projects/client_thread_posted` (email, WhatsApp)
- `client_thread.replied` → `projects/client_thread_replied` (email, WhatsApp)
- `change_request.review_requested` → `projects/change_request_review_requested` (email, WhatsApp)
- `change_request.decided` → `projects/change_request_decided` (email, WhatsApp)
- `task.automation_notice` → `projects/task_automation_notice` (email, WhatsApp, one party per recipient id)
- `task_due` → `deadline_reminder_push` template

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

### Hadia (gift registry)

**Integration Type**: Events (NATS), stream `hadia` (subjects `hadia.>`) owned by hadia-api, and
`hadia_secret` for one-time codes.

**Consumers**:
- `cmd/worker/hadia_consumer.go`, durable `notifications-hadia`: every guest, owner and Hadia staff
  message (`hadiaMappings`), templates under `templates/email/hadia/` and `templates/whatsapp/hadia/`.
  Owner messages carry the owner's own contact details; staff messages go to the tenant's contact.
- `internal/modules/events/hadia_ops_health.go`, durables `notif-hadia-ops-health-daily` and
  `notif-hadia-ops-health-weekly`: the platform health reports. The worker consumer acks those two
  as unmapped.

**Added 12 November 2026** (hadia-api 41ae1d7):
- `hadia.payout.number_needed`: a registry closed at expiry with money still owed and no verified
  payout number. The owner gets `hadia/payout_number_needed` (email, essential) and
  `hadia_payout_number_needed_v1_btn` (WhatsApp, params name, registry, amount; button the payouts
  page from `owner_url`). The amount is formatted in the payload's `currency`. The same close opens a
  high priority support and finance case; `hadia.case.opened` already tells Hadia's staff (email, and
  WhatsApp for high and urgent), so this event sends nothing to staff.
- `hadia.ops.health_daily` and `hadia.ops.health_weekly`: responders `platform_ops`, so they take the
  existing ops alert path (`sendOpsAlert` in `gateway_health_alert.go`, shared with the payment
  provider outage alert): email to `PLATFORM_ALERT_EMAILS` (`platform/hadia_health_daily` and
  `platform/hadia_health_weekly`, locked), a one-line summary to the Slack webhook in auth-api's
  `platform_alert_channels`, and the ops WhatsApp numbers there (`broadcast_service_notice_v1`) only
  for a daily report that needs a person: registries whose gift log differs from treasury, failed
  payouts in 24 hours, urgent or overdue cases, failed outbox events, or an outbox event unsent for
  ten minutes. The weekly report never goes by WhatsApp. The failed payout count is shown as
  approximate when the payload says so (`failed_payouts_24h_approximate`), consumer lag only when
  hadia-api read it, and the weekly money in KES with the payload's `currency_scope` note. Both
  durables start at new messages, so a first rollout does not replay the week the stream keeps.

**Added 15 November 2026** (Gift Hadia, hadia-api a4fad1b8, Sprint 20):
- `hadia.referral.rewarded`: an owner referrer (`reward_kind` `owner_step`) gets
  `hadia/referral_rewarded` by email at `owner_email` (essential): Hadia's commission on their
  registries is lower by `step_points`, never below `floor_rate`, what it applies to for guest-pays
  and owner-pays registries, and until when (`reward_expires_at`, or no end date); button the Gift
  Hadia page (`owner_url`). hadia-api sends no resulting rate, so the email does not state one. A
  guest referrer's reward carries no contact and sends nothing.
- `hadia.referral.spike`: 3 or more rewards for one referrer in 7 days. `hadia/referral_spike` by
  email to the tenant's contact (Hadia staff, essential), with the referrals report as its link.
- `hadia.referral.qualified` is acked as unmapped: the rewarded event follows it in the same
  transaction.
- WhatsApp for both waits for Hadia's own number. Then the rewarded mapping takes `PhoneKey`
  `owner_phone`, a `templates/whatsapp/hadia/referral_rewarded.txt` and a template declared in
  `templatesync/templates.json`; the spike takes a WATemplate with a `WhatsAppIf` as `case.opened` does.

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

#### Managed link (number in the Codevertex portfolio)

A tenant's number can be connected two ways. Embedded Signup stays the normal path: the tenant connects its own number in its own business portfolio, and I store that business's token. The managed link is the second path, for a tenant whose WhatsApp account lives inside the verified Codevertex Africa Limited portfolio (owner decision 2026-11-13). Meta's Embedded Signup never offers the portfolio of the business that owns the app, by design, so such an account can only be set up by hand in WhatsApp Manager and then linked here. Hadia is the first.

Use Embedded Signup when the tenant has, or can create, its own business portfolio; its own name shows in chats once it verifies that business with Meta, and its sending limits grow with its own verification. Use the managed link when we run the account for the tenant inside our verified portfolio, so it starts with our verification and our payment method.

Steps in WhatsApp Manager (Business Settings of the Codevertex Africa Limited portfolio):

1. Create a new WhatsApp account inside the verified portfolio, named after the tenant.
2. Add the tenant's phone number to that account and verify it by SMS or call.
3. Attach the payment method under the account's payment settings (paid templates fail with #131042 without one).
4. Under System users, assign the platform system user to the new WhatsApp account with full control. Without this every Meta call answers "missing permissions".
5. Copy the Phone Number ID and the WhatsApp account ID from the account's API setup page.

Then, signed in as a platform owner and acting for the tenant, open Settings > Providers > WhatsApp and use "Link a number from our portfolio" with those two IDs. The UI calls `POST /api/v1/billing/whatsapp/managed-link` with `{"phone_number_id": "...", "waba_id": "..."}` and the tenant in `X-Tenant-ID`. With All Tenants selected (or the codevertex tenant picked) the link goes to the platform tenant itself, which is how the platform's own number is linked again after a Disconnect. Anyone who is not a platform owner gets 403.

With the platform access token (`LoadPlatformMetaCredentials`), the handler:

- refuses the platform's own phone number for any tenant but the platform (409). The platform's own WhatsApp account is our portfolio and holds other tenants' numbers too, so the account alone is never a reason to refuse;
- refuses a number already saved on another tenant, and an account saved on another tenant that connected it itself (Embedded Signup). An account held only by managed links (`token_source=platform`) is the portfolio and is no conflict; the platform tier rows are not a tenant link;
- reads the number (`display_phone_number`, `verified_name`, `platform_type`, `status`, `code_verification_status`) and the account's `phone_numbers`, and refuses a number that is not in that account (400);
- subscribes our app to the account's webhooks, and registers the number for Cloud API unless Meta reports it `CLOUD_API` and `CONNECTED` right now. `platform_type` alone is not enough: it stays `CLOUD_API` after a deregister, and on 15 November 2026 a relink that skipped register left Urban Loft and the platform number failing every send with 133010 "Account does not exist in Cloud API";
- registers with the PIN saved for the tenant (`registration_pin`, encrypted) when there is one, else a new one, and saves the PIN used. Two step verification keeps the first PIN on Meta, so a second PIN would be refused;
- saves `phone_number_id`, `waba_id` and `token_source=platform` on the tenant, removes any old tenant `access_token` (except on the platform tenant, whose row can hold the platform token itself), clears the settings cache and starts the template and profile auto-syncs, then answers like Embedded Signup (including `payment_method_missing`).

When Meta refuses a call, the API answers 422 with Meta's own words: `error` (what failed, Meta's `error_user_title` and `error_user_msg`, and what to do), plus `meta_title`, `meta_message`, `meta_code`, `meta_subcode` and `hint`. The token is never echoed. Known hints: subcode 2593005 (number not verified) says to verify the number with its SMS or voice code in WhatsApp Manager and link again; code 133005 (PIN mismatch) says to reset two step verification for the number in WhatsApp Manager and link again. 502 is kept for Meta not answering at all. Embedded Signup uses the same register rules and answers.

**Registering again, disconnecting and linking again.** A number can be disconnected and connected again any number of times:

- `POST /api/v1/billing/whatsapp/reregister` (platform owners, for the tenant in `X-Tenant-ID` or the platform tenant) registers the linked number again with its saved PIN, without disconnecting it. The panel shows it as "Re-register number". A send refused with 133010 carries a hint pointing to it.
- `GET /api/v1/billing/whatsapp/registration` reports the number's live state on Meta (`registered`, `status`, `platform_type`), so the panel shows a number linked here but not registered on Cloud API as "Number not registered on Meta".
- `DELETE /api/v1/billing/whatsapp/connection` removes the tenant's number, account, token and marker rows; the registration PIN stays for the next registration. What it releases on Meta: never the platform tenant's own number, never a number another tenant or the platform tier also holds, and never unsubscribes an account another tenant or the platform also uses. `release=true` (the panel's "also release the number from Cloud API" box, off by default) deregisters a number that is not shared; `release=false` keeps it registered so it can be linked again at once; with no parameter a tenant's own Embedded Signup account is released as before.
- Sends for the platform tenant fall back to the platform tier credentials (Platform > Providers > WhatsApp) when its own rows are missing, so a Disconnect on the platform tenant no longer stops platform messages. No other tenant ever gets this fallback; the shared number allowlist rule is unchanged. Platform template management reads the same merged credentials.

**Verifying a number again in the app (added 2026-11-15).** A number deregistered for a while can need re-verification before Meta registers it again: register answers 133006 "Phone number re-verification needed". The owner verifies it from the panel instead of WhatsApp Manager:

- Link and Re-register answer 422 with `needs_verification: true` on 133006 (Link still saves the number, so it can be verified in place). `GET /billing/whatsapp/registration` also reports `code_verification_status` and `needs_verification`.
- `POST /api/v1/billing/whatsapp/verification/request` with `{"code_method": "SMS" | "VOICE", "language": "en_US"}` asks Meta (`{phone_number_id}/request_code`) to send a code. At most one request a minute and five an hour per tenant (429 with `retry_after_seconds`).
- `POST /api/v1/billing/whatsapp/verification/verify` with `{"code": "123456"}` checks it with Meta (`{phone_number_id}/verify_code`), then registers the number with its stored PIN as Re-register does, and answers with the live state (`registered`, `status`, `code_verification_status`). At most ten checks per 15 minutes per tenant. The code is never logged or echoed.
- Both are platform owners only, for the tenant in `X-Tenant-ID` or the platform tenant. Meta refusals answer 422 like the others, with hints for 136024 (Meta could not check the code) and 136025 (wrong or expired code).

`token_source=platform` is what makes sends, the template sync, the profile editor and Disconnect use the platform system user's token with the tenant's own number and account IDs (`Manager.tenantWhatsAppSettings`). Without that marker the platform token is never used for a tenant number. Only a platform owner can set the marker through the settings form, and a tenant cannot change the number or account of a managed link there. Disconnect removes the marker with the other connection rows, and a later Embedded Signup removes it too, so the tenant's own token takes over.

**A number ID saved on its own (fixed 2026-11-14).** Hadia's number (phone number ID 1471029032759769) was created in WhatsApp Manager and its ID pasted into the old "Manual Setup: Phone Number ID" field. That form saved only `phone_number_id`: no `waba_id`, no token and no `token_source`. The settings page then said "connected" while every send was skipped ("no WhatsApp number registered for this tenant"), and the template and profile pages answered 400 asking for an Embedded Signup reconnect. I closed that path:

- The UI no longer has the manual field. A platform owner uses "Link a number from our portfolio" with both IDs; a tenant connects with Embedded Signup. A saved number ID without its account now shows as "Number saved but not linked yet", and the profile and templates cards stay hidden until it is linked.
- `POST /api/v1/providers/settings` refuses (422) a whatsapp/meta_cloud save that would leave a tenant with a `phone_number_id` and no `waba_id`, no token and no `token_source=platform`. A platform owner is told to use the portfolio link with the two IDs; anyone else is told to connect with Embedded Signup or ask Codevertex support. The platform's own row is not affected.
- A tenant already in that state gets `ErrTenantNumberUnlinked`: the template and profile routes answer 409 saying the number has to be linked from our portfolio, and a send still counts as `ErrNoWhatsAppNumber` (the worker skips it) with the same explanation in its error.

Running the managed link for that tenant overwrites the half-saved rows, so nothing else needs cleaning up first.

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
| always (shared) | `auth_`, `subscription_`, `credits_`, `broadcast_`, `occasion_`, `notifications_test_` (the provider Test button's template) |
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

#### WhatsApp costs report (platform owners)

I decided on 2026-11-12 to build a platform cost report: for every WhatsApp Business Account (WABA) we send from, and every number on it, what Meta charged next to what the tenant paid us in WhatsApp credits, so I can see the margin per tenant. It lives at Platform > WhatsApp costs in notifications-ui.

- Route: `GET /api/v1/platform/whatsapp/costs?from=YYYY-MM-DD&to=YYYY-MM-DD` under the `/platform` group (super admin role), and the handler also requires the signed `is_platform_owner` claim because the report shows every tenant's money. The UI tab gates on that claim too (`isPlatformOwnerClaim`), not on the codevertex slug. Both dates are whole UTC days and inclusive; without them the report covers this month so far. At most 366 days.
- Accounts: the platform tenant's own whatsapp/meta_cloud row (its `waba_id`, read with the platform Meta token from `LoadPlatformMetaCredentials`) and every tenant row holding a `waba_id` (`Manager.WhatsAppCostAccounts`). Rows are grouped by WABA. Tokens follow the same managed link rule as sending (`tenantWhatsAppSettings`): a tenant row with `token_source=platform` uses the platform token, any other row only its own, and a row with none shows "no token" on its block. That is how a tenant number inside the platform WABA (Urban Loft's today) is covered.
- Meta's side, per WABA: `GET /{version}/{waba_id}?fields=currency,pricing_analytics.start(S).end(E).granularity(DAILY).dimensions(PHONE,PRICING_CATEGORY)` with the token as a bearer header, then `GET /{version}/{waba_id}/phone_numbers?fields=id,display_phone_number,verified_name` to name the numbers. `start` and `end` are UNIX timestamps (end is the day after `to`). Each data point carries `phone_number`, `pricing_category`, `volume` and `cost`, so the shared platform WABA splits per number by matching the digits of `phone_number` to `display_phone_number`. The number's owner is the provider row naming its `phone_number_id`; any other number on the platform WABA is the platform's.
- Meta facts I relied on (checked 2026-11-13): granularity is `HALF_HOUR`, `DAILY` or `MONTHLY`; dimensions are `COUNTRY`, `PHONE`, `PRICING_CATEGORY`, `PRICING_TYPE` and `TIER`; categories include `MARKETING`, `MARKETING_LITE`, `UTILITY`, `AUTHENTICATION`, `AUTHENTICATION_INTERNATIONAL`, `SERVICE` and `REFERRAL_CONVERSION` (the report folds the lite and international ones into their parents and anything else into "other"); cost is approximate and "in your Messaging account's currency", which the WABA's `currency` field names; the lookback is one year since 2025-12-01; `COST` is not returned for accounts on a Solution Partner's credit line. Meta states no data delay, only that the figures are approximate and can differ from the invoice, so the page says the latest day may still be filling in and that the invoice is the final word. Sources: [WhatsApp analytics](https://developers.facebook.com/docs/whatsapp/business-management-api/analytics), [WhatsApp Business Account reference](https://developers.facebook.com/docs/graph-api/reference/whats-app-business-account/), [pricing](https://developers.facebook.com/documentation/business-messaging/whatsapp/pricing).
- Our side: subscriptions-api `GET /api/v1/s2s/credits/whatsapp-usage?from=&to=` (service key). Per tenant and wallet (`whatsapp_marketing`, `whatsapp_utility`, `whatsapp_authentication`, `whatsapp_service`, and `whatsapp_quota` for the retired tiers' shared allowance): credits bought and the KES paid for them, credits used net of refunds, overage KES, and a credits value (credits used at today's unit price plus overage). The admin overview and tenant list there only cover the current month without per tenant KES, so this endpoint is new.
- Money: Meta's cost is shown in the WABA's currency and in KES, converted with treasury's rate the same way marketflow-ai does it (`internal/fxrate`: `GET {TREASURY_API_URL}/api/v1/s2s/{FX_PLATFORM_TENANT_SLUG}/exchange-rates/convert?from=USD&to=KES&amount=1` with the internal service key, cached in Redis for an hour with a last good fallback; defaults `https://booksapi.codevertexafrica.com` and `codevertex`). The margin is credits value less Meta's cost in KES. When no rate can be read the row says "rate unavailable" and the margin is left out; the totals in KES need every WABA read and converted.
- A WABA whose pricing data points carry no `phone_number` (Meta did not split by number) gets one "whole account" row with its total, logged once per WABA, and the report still renders.
- Tenants with credit movement and no number of their own send from the platform number. They are listed under "Tenants sending from the platform number" and their Meta cost is inside the platform number's row.
- Resilience: Meta and subscriptions-api are called on a context detached from the request (60 seconds overall, 20 seconds per Meta call), WABAs in parallel. A WABA Meta refuses (or one with no token) shows Meta's message on its block and the rest of the report still renders; a subscriptions-api failure shows as `credits_error`.
- Cache: Redis key `notifications:whatsapp-costs:v1:{from}:{to}` for one hour, or five minutes when any part failed, so a passing outage is not shown for an hour. `cached: true` marks a saved copy.

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

**Inherited settings (added 2026-11-17)**: a tenant without its own email or SMS account sends with
the platform's (Platform > Providers), as `internal/providers/provider_scope.go` resolves it: an SMTP
account belongs to the tenant once it saves its own host, a Brevo or Africa's Talking account once it
saves its own API key. `GET /api/v1/providers/settings?provider_type=&provider_name=` still returns the
tenant's own values in `settings`, and for such a tenant adds:

```json
{
  "inherited_from": "platform",
  "effective": {"host": "...", "port": "465", "username": "...", "from": "...", "reply_to": "..."},
  "inherited_keys": ["from", "host", "port", "reply_to", "username"]
}
```

`effective` never holds a password or API key. The platform account's host, port and username are
only returned to a platform owner; any other tenant gets the sender values (from, reply-to, sender
name), with its own on top of the platform's. Settings > Providers shows these read only, marked
"Inherited from Platform > Providers", until the user picks "Use custom settings for this tenant".
Saving the tenant form writes only the tenant's own rows and skips empty values, so the platform's
values are never changed or blanked by it. WhatsApp never inherits (a tenant without a number of its
own is never sent through the platform's).

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

