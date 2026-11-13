# Template mapping audit, 12 November 2026

## Addendum, 13 November 2026: tenant mail sent from the platform account

Kura's case emails arrived from no-reply@codevertexafrica.com with Codevertex branding, although
Kura's own SMTP account was configured and active. The delivery log filed them under the codevertex
tenant.
- truload-backend sends with a bng_ platform service key, which auth-api validates as service
  "platform" with the superuser role.
- `TryDeveloperKey` kept only the key's tenant id and dropped its roles and service.
- Since the 11 November security fix only a privileged caller may name another tenant, so the tenant
  in the body was ignored and the send used the platform account.

Fixed in 3c223b4: the validated key now reaches the request as claims, the same way auth-client's
API-key path does. A platform service key is privileged again; an ordinary tenant developer key
stays pinned to its own tenant.

A tenant's grace email read "Your subscription for has reached the end of its billing period" with
an empty Plan line. The event never carried the plan and the consumer read a key that was not
there. `html/template` prints a missing key as nothing, so these mistakes only show in the inbox. I
checked every consumer mapping against its template and against what the publishing service sends.

## What the renderer fills for every email

`cmd/worker/main.go` adds `brand_name` (and `brand`), `brand_email`, `brand_phone`,
`brand_website`, `brand_logo_url`, `brand_primary_color`, `brand_secondary_color`, `app_url` (when
the caller did not set it), `getting_started_link` and `sent_at`. Nothing fills `name` or
`action_link` centrally, and SMS, WhatsApp and push get nothing added.

## Fixed

| Email | Was | Fix |
|-------|-----|-----|
| Subscription grace, trial ended | Blank plan in the heading and the summary | subscriptions-api sends `plan_name`; the consumer reads it before `plan_code` |
| Subscription created | Plan shown as its code | Reads `plan_name` first |
| Subscription cancelled | Blank plan and access date | subscriptions-api sends the plan and `access_until` |
| Access blocked (`expired`), payment required, dormancy warning and suspension | Never sent: no mapping | New templates `subscription/subscription_expired` and `subscription/account_notice` |
| Low stock (second copy) | Almost entirely blank, and sent twice | The duplicate handler in `internal/modules/events/subscriber.go` is removed; the worker's alert stays |
| Low stock and stock out | Warehouse shown as a UUID, empty stock-out time | inventory-api sends `warehouse_name` and `detected_at` |
| Event ticket (second copy) | Sent twice, one without the PDF link | The worker copy is removed; the subscriber's, with the link, stays |
| Project milestone | "Overall Progress: %" and a raw date | The builder sets `project_progress`; the date is formatted |
| Refund completed | "Hello ," | Greets "there" when no name is sent |
| Payout completed | Empty Date | Shows the settlement period end |
| Library fine | Raw member id, empty reason, and a second email whenever a payment started | library-api no longer republishes on payment start; the email shows the member's name and the reason in words |
| Library membership due | Empty expiry | library-api sends `expires_at`; the line hides when absent |
| Library late serial | Empty issue number | library-api sends `issue_no` |
| Scheduled order | Raw timestamp | Shown as a date and time in Kenya time |
| Purchase order received | No supplier name | inventory-api sends `supplier_name` |
| Customer delivery emails (`delivery_consumer.go`) | Never sent: logistics-api sent no customer, `logistics/delivery_en_route` had no template, and the estimated arrival, delivered time, proof of delivery, failure reason and support button were never filled | logistics-api keeps the order's customer in the task metadata and sends `crm_contact_id` (CRM-linked) or `customer_email`, `customer_name`, `customer_phone` on the task events listed in logistics-api's docs/integrations.md, plus `estimated_arrival` and `delivered_at`, and the rider's name and phone on `task.en_route_dropoff` / `task.en_route`. The consumer resolves a CRM contact through `internal/contacts` (retrying while the CRM is down), greets the customer by name, links to the public tracker, describes the proof of delivery and links the tenant's support email. The Rider line shows only when the event names a rider, and the rider's own job alert goes out on the first delivery only, so a CRM retry never repeats it. New template `logistics/delivery_en_route`; optional lines are guarded. Covered by `cmd/worker/delivery_template_test.go` |
| Customer delivery emails for pos-api orders | Never sent: pos-api's `buildDeliveryTaskRequest` passed only the customer's name and phone | pos-api sends `crm_contact_id` for a CRM-linked customer, else the customer's email (with the name and phone), on the S2S task create |

## Delivery email decisions, 12 November 2026

- **(a) Ordering orders**: the `source_service` `ordering` skip in `delivery_consumer.go` stays.
  Ordering owns the customer messages for its own orders.
- **(b) pos-api passes the customer**: `buildDeliveryTaskRequest` sends `crm_contact_id` for a
  CRM-linked customer, else the customer's email, with the name and phone as before. A linked
  customer goes out by id only, the same rule pos-api's own events follow.
- **(c) Estimated arrival**: shown only while it is still in the future. logistics-api's
  `estimatedArrival` skips a zero or past requested drop-off or SLA due time.
- **(d) Failed deliveries**: the customer keeps getting the failed email, with the reason and the
  tenant's support contact.

## Paths that still do not send

These render nothing blank because they never send at all. Each needs a decision or publisher
work before it is switched on.

- **Delivery emails for ordering orders**: skipped on purpose, decision (a). Ordering owns the
  customer conversation for its own orders (out for delivery, delivered; a failed delivery is a
  staff alert there), so `delivery_consumer.go` sends the logistics delivery emails only for tasks
  from other sources (pos-api, dispatcher-created and S2S tasks). The estimated arrival shows only
  when the task has a requested drop-off or SLA due time still ahead; the live routing ETA is not
  on the status events.
- **Maskani visitor pass by email**: maskani-api sends only `visitor_phone`, so the pass goes out on
  WhatsApp only. That matches the gate flow, which collects a phone number.
- **Supplier PO confirmation**: inventory-api does not send `supplier_email`, so suppliers get no
  email on goods receipt. Decided 12 November 2026: keep it off for now; tenants send purchase
  orders to suppliers themselves. Turning it on later means adding `supplier_email` to
  `purchase_order.received` behind a per-tenant switch.
- **Payroll disbursed SMS**: there was no `templates/sms/payroll/disbursed` template and no publisher
  of `treasury.payroll.disbursed` in treasury-api or erp. I removed the dead subscriber on 12
  November 2026; it gets built with payroll disbursement through treasury. The payroll emails ERP
  requests (`payroll/payroll_disbursed` and the others in docs/integrations.md) are a separate path
  and still work.
- **Library e-book expired**: declared in library-api but never published.

## Not checked against a publisher

Ticketing (no publisher found in the workspace), ISP billing, the ERP generic
`erp.notification.requested` (the caller picks the template), and the gateway health, PayHero and
sale recipient alerts, which have their own tests.
