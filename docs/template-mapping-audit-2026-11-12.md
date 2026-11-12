# Template mapping audit, 12 November 2026

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

## Paths that still do not send

These render nothing blank because they never send at all. Each needs a decision or publisher
work before it is switched on.

- **Delivery emails to customers** (`delivery_consumer.go`): logistics-api never sends
  `customer_email`, so every customer delivery email is skipped. `logistics/delivery_en_route` also
  has no email template; one is needed before `customer_email` is added.
- **Maskani visitor pass by email**: maskani-api sends only `visitor_phone`, so the pass goes out on
  WhatsApp only. That matches the gate flow, which collects a phone number.
- **Supplier PO confirmation**: inventory-api does not send `supplier_email`. Sending it would start
  emailing suppliers automatically on goods receipt, which is the owner's call.
- **Payroll disbursed SMS**: there is no `templates/sms/payroll/disbursed` template and no publisher
  of `treasury.payroll.disbursed` in treasury-api or erp.
- **Library e-book expired**: declared in library-api but never published.

## Not checked against a publisher

Ticketing (no publisher found in the workspace), ISP billing, the ERP generic
`erp.notification.requested` (the caller picks the template), and the gateway health, PayHero and
sale recipient alerts, which have their own tests.
