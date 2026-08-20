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

**Template Mapping**:
- `order_created` → `order_confirmation_email` template
- `order_status_changed` → `order_status_update` template
- `order_ready` → `order_ready_sms` template

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

