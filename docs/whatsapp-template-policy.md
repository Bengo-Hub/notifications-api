# WhatsApp Template and Link Policy

Rules for every WhatsApp message the platform sends, and for every template added to
`internal/whatsapp/templatesync/templates.json`. Tests enforce them.

## 1. Links are buttons, never text

A WhatsApp message never shows a raw URL. A link always goes out as a tappable button, like
"Track Order" or "Leave Feedback" under the message.

| Send type | How the link is attached |
|---|---|
| Template (business-initiated, any time) | A template with a `URL` button whose URL is a fixed domain plus a `{{1}}` suffix (`_btn` templates). The code passes the suffix as `template_button_param`. |
| Free-form (inside the customer's 24h reply window) | An interactive `cta_url` message: `cta_button_text` + `cta_button_url` metadata. The worker adds this automatically from the message's link field (`order_link`, `track_link`, `review_link`, `action_link`, `download_link`, ...; see `cmd/worker/whatsapp_link_policy.go`). |

Consequences:

- Template bodies have no link variable. The button carries it.
- WhatsApp text bodies under `templates/whatsapp/**/*.txt` never include a link.
- No raw-link fallback template. If a button template cannot be sent (for example still in Meta
  review), WhatsApp is skipped for that message; email and SMS still go.

## 2. Button domains

Meta fixes a button's domain when the template is approved; only the suffix varies per send.

| Links to | Button URL | Suffix |
|---|---|---|
| Customer order page, tracking, review, pay | `https://ordering.codevertexafrica.com/{{1}}` | `<tenant-slug>/orders/guest/<id>[?rate=1]`, `<slug>/track/<code>` |
| POS online orders queue | `https://pos.codevertexafrica.com/{{1}}` | `<tenant-slug>/online-orders` |
| POS receipt | `https://r.codevertexafrica.com/{{1}}` | receipt short code |
| Platform invoice (subscription, support) | `https://books.codevertexafrica.com/{{1}}` | `i/<public token>` (the invoice page), else the pay page path |
| Maskani portal and console | `https://maskaniapp.codevertexafrica.com/{{1}}` | `<tenant-slug>/portal`, `<slug>/portal/purchase`, `<slug>/portal/walk-ins/<id>`, `<slug>/security/incidents/<id>`, `<slug>/works/<id>` |
| Hadia gift registry (one client tenant on its own domain, so no slug) | `https://hadiafrica.com/{{1}}` | `r/<code>` (guest page), `r/<code>/reserved/<token>` (reservation manage page), `app/registries/<id>` (owner), `app/registries/<id>/gifts` (gift log), `app` (owner dashboard, the daily gift summary when it covers several registries). `cmd/worker/hadia_consumer.go` sends no button for a link on any other host |
| Rider app (a rider's own screens) | `https://riderapp.codevertexafrica.com/{{1}}` | `<tenant-slug>/profile` |
| Logistics app (the business's dispatch console) | `https://logistics.codevertexafrica.com/{{1}}` | `<tenant-slug>/vehicles` |
| Digitika and the company website (enrollment page, student and parent portal, certificate check, enquiry reply) | `https://codevertexafrica.com/{{1}}` | `digitika/success?reference=<ref>`, `student`, `digitika/parent`, `certificates/<token>`, `services` (website enquiry reply) |
| Projects app (task mentions and assignments) | `https://projects.codevertexafrica.com/{{1}}` | the event's `action_path` without its leading slash, `<org-slug>/projects/<id>/tasks?task=<task id>` |

## 2b. One notice, several parties

A notice that reaches more than one person (the rider and the business, a customer and the
business, every responder on an incident) is sent with `sendToParties` (`cmd/worker/notify_parties.go`):
one `partyNotice` with a `noticeParty` per recipient, each with its own contacts, channels (email
and WhatsApp by default; SMS and push only when the caller asks), data and WhatsApp template. The
helper owns the shared rules: placeholder emails are skipped, a `_btn` template never goes without
its link suffix, phone channels get the tenant's dial code, idempotency keys are
`<base>-<channel>-<party>`, and when the caller passes the preferences Gate the tenant's channel
choices and drafted templates apply (`fanOutTargets`). maskani and logistics vehicle insurance use it.

Platform billing messages (`subscription_invoice_ready_v1_btn`, `subscription_payment_due_v1_btn`)
are sent from the platform's number (`senderScope: platform`), so the tenant's own WhatsApp plan
and quota do not apply. They go to ONE number from auth-api
(`GET /api/v1/s2s/{tenant}/billing-contact`, ordered: the tenant administrator, then the main
outlet, then the tenant's phone), next to the email (`cmd/worker/billing_whatsapp.go`). The other
numbers are backups in metadata `fallback_to`: a send Meta refuses moves to the next number at
once, and a send Meta accepts but later reports `failed` (webhook) is sent to the next number then
(`internal/messaging/whatsapp_fallback.go`, parked in Redis under Meta's message id for 24 hours).

The shared apps serve every tenant under its slug, so a tenant on its own domain still gets a
working button: the suffix is the path of its link (`buttonURLSuffix`, `posButtonSuffix`).

## 3. Template formatting (Meta review rules)

- Body variables numbered `{{1}}`..`{{n}}` without gaps, one `example` value each.
- A body never starts or ends with a variable; close with a short sentence.
- Enough words around the variables: Meta rejects a body with too few as error 2388293 ("parameters words
  ratio"). Three variables and six other words was refused. Aim for at least four words per variable.
- Emphasis with `*bold*` on the key value (order number); blank lines between sections.
- `https` button URLs, at most one `{{1}}`, at the end, with one example; button text up to 25
  characters.
- A new wording is a new template name (`_v2`, `_v3`, ...); approved names are never edited in
  place. Deleted names cannot be reused for about four weeks.
- A message to someone who is not the tenant's own customer (a visitor, a guest) must say who arranged
  it for them and read as a confirmation of that arrangement: `maskani_visitor_pass_v3` ("your visit
  reference is X, show it to the guard") was refused as `INCORRECT_CATEGORY` twice; v4 names the host
  and the unit ("Jane has booked your visit to unit B07 at Shaba Village... your booking number is
  482 915") and shows a six-digit number in two groups so it never reads as a sign-in code.
  `hadia_member_invited_v1_btn` ("invited you to help run the registry") was refused the same way on
  14 November 2026; v2 says who added the person and that they are now a co-owner, so it reads as a
  notice about access already granted rather than an invitation. It also drops "sign in" from the body
  and the button: the body says "open Hadia with this phone number" and the button reads "Open Hadia".
- Meta rejects a UTILITY template as `INCORRECT_CATEGORY` when the wording reads as a login credential or
  a promotion. Keep these words out of bodies: password, login, username, sign in, one-time code, gate
  code, welcome, enjoy, thank you for choosing. Say "account details were sent to you separately" and
  "pass number" instead, and send the real credential by email. A one-time code uses the
  `AUTHENTICATION` template, never UTILITY. An example value counts too: a bare six digit number beside
  "pass number" was refused, so give a reference an example like `VIS-482915` and word it as a confirmation.
- Category `UTILITY` for transactional messages; `AUTHENTICATION` uses Meta's fixed format.

## 3b. Category: what Meta bills, not what we submit

Meta can approve a UTILITY template as MARKETING, and from then on bills every send at the marketing
rate (about five times utility) and holds it to its per person marketing limit: after one or two
marketing messages a person stops receiving them, and each later send fails in the status webhook with
**131049** ("not delivered to maintain healthy ecosystem engagement") although the send call returned
an id. Meta can also move a template that was approved as utility later.

On 16 November 2026 Meta held 9 of our UTILITY templates as MARKETING on the shared WABA. The Test
button's template was one of them, so every test after the first to the same phone failed on Hadia's and
Urban Loft's numbers. What tipped each one, and what the rewrite does:

| Moved template | Why Meta read it as marketing | Replacement |
|---|---|---|
| `notifications_test_message_v1` | a generic notice tied to nothing the reader did | `notifications_test_message_v2`: confirms the test the person asked for, with its time and reference |
| `credits_balance_low_v1_btn` | "Buy more" and a "Buy credits" button (upsell) | `credits_balance_low_v2_btn`: a balance statement, "View balance" |
| `hadia_registry_published_v1_btn` | "Share it with family and friends" | `hadia_registry_published_v2_btn`: the registry is published and its link is active |
| `hadia_change_requested_v1_btn` | "would like to" with the change as a bare variable | `hadia_change_requested_v2_btn`: "Approval needed on your registry" |
| `hadia_legal_updated_v1_btn` | a policy notice that reads as a newsletter | `hadia_legal_updated_v2_btn`: an account notice about the terms of the reader's account |
| `pos_loyalty_referral_v1` | "Use them on your next visit" | `pos_loyalty_referral_v2`: an account credit with the balance |
| `ispbilling_expiry_reminder` | "renew and stay connected", "thank you for staying with us" | `ispbilling_expiry_reminder_v2` (not sent yet: ISP notices go by SMS and email) |
| `ispbilling_subscription_expiring` | "Renew now to stay connected" | `ispbilling_subscription_expiring_v2` (same) |
| `broadcast_service_notice_v1` | the whole body is a free text variable sent to many people | kept, recorded as `"meta_category": "MARKETING"` so credits bill it as marketing |

Rules that follow:

- A utility body states a fact about the reader's own account, order, payment or request, with its
  specific detail (number, amount, date, reference). No invitations to buy, renew, visit, share, review
  or "stay connected", no thanks for loyalty, and no button that sells.
- A body that is mostly one free text variable is marketing to Meta whatever its wording.
- `category` in `templates.json` is what we submit. When Meta holds a template under another category,
  record it as `meta_category`: credits bill that (`TemplateDef.Billed`) and the audits stop flagging it.
- Check Meta's verdict after every sync and once a month: `GET /api/v1/platform/whatsapp/templates/categories?recategorized=true`
  (platform owner) lists every template on every WABA we send from whose Meta category differs from ours.
  The tenant's own view (`GET /api/v1/settings/whatsapp/templates`) shows `meta_category` and `recategorized` too.
- A rewrite under a new name can ship before Meta approves it if `internal/providers/whatsapp/superseded.go`
  maps it to the approved template it replaces (with the parameter order of the old one): until approval
  every send falls back to the old template. Drop the entry once the rewrite shows APPROVED as UTILITY.

## 4. Publishing

1. Add the template to `templates.json` (button variant for anything with a link).
2. `go test ./internal/whatsapp/... ./cmd/worker/` (manifest lint, parameter counts, link policy).
3. After deploy: notifications-ui Templates > WhatsApp sync, dry run first (or
   `POST /api/v1/platform/whatsapp/templates/sync {"dry_run":true,"only":[...]}` as a platform
   admin), then submit. The results show Meta's review status (`meta_status`).
4. Switch the code to the new name only once it shows `APPROVED`, or accept that WhatsApp is
   skipped for that message until it is.

Templates waiting for a Meta sync after their deploy:

| Template | Sent for |
|---|---|
| `projects_task_mentioned_v1_btn` | `project.task.mentioned` with a comment excerpt |
| `projects_task_mentioned_short_v1_btn` | `project.task.mentioned` without an excerpt |
| `projects_task_assigned_v1_btn` | `project.task.assigned` |
| `notifications_test_message_v2` | the provider Test button (falls back to v1 until approved) |
| `credits_balance_low_v2_btn` | `billing.balance.low` to tenant admins (falls back to v1) |
| `hadia_registry_published_v2_btn` | `registry.published` (falls back to v1) |
| `hadia_change_requested_v2_btn` | `registry.change_requested` (falls back to v1) |
| `hadia_legal_updated_v2_btn` | `legal.updated` (falls back to v1) |
| `pos_loyalty_referral_v2` | `loyalty.referral_earned` (falls back to v1) |
| `ispbilling_expiry_reminder_v2`, `ispbilling_subscription_expiring_v2` | not sent yet |

Until Meta approves them the task emails still go and WhatsApp is skipped. The `_v2` rewrites of 16 November
2026 keep sending through their approved v1 until then (`superseded.go`).

## 5. MarketFlow assistant templates (Sprint 17, for the owner to submit)

MarketFlow's WhatsApp assistant writes to business owners from the platform number. Inside the
24 hour window it sends free-form messages with reply buttons. Outside the window it needs the
templates below. They are **not** in `templates.json` yet, so a sync cannot submit them by
accident: the owner submits them in WhatsApp Manager on the platform WABA (Codevertex), and they
move into the manifest once approved.

MarketFlow asks `GET /api/v1/s2s/whatsapp/templates/status?tenant_id=<platform tenant>&names=...`
(service key) before each use and sends a template only when Meta shows it `APPROVED`. Until then
the message goes by email only (the daily brief), or the requester sees it in the Studio and gets
the WhatsApp message the next time they write (video ready, publish result). The names can be
changed in marketflow-api config (`MF_WA_TPL_*`) if Meta refuses one and a `_v2` is submitted.

Every template: category `UTILITY`, language `en_US` (English), one `URL` button on
`https://marketflow.codevertexafrica.com/{{1}}`. Bodies have no link and no newline in a variable.
`{{1}}` is always the business name (MarketFlow does not hold each owner's first name).

| Name | Body | Example values | Button text | Button example suffix |
|---|---|---|---|---|
| `mf_video_ready_v1_btn` | `Hi {{1}},\n\nYour video ad *{{2}}* has finished rendering and is waiting for your review.\n\nOpen it to approve it, post it or change a scene.` | `Jua Kali Salon`, `Weekend offer` | `Review video` | `jua-kali-salon/studio/2f6c1a9e-7b3d-4c55-9a1e-0d4b6f2e8c71` |
| `mf_publish_result_v1_btn` | `Hi {{1}},\n\nYour video ad *{{2}}* was sent to your accounts: {{3}}.\n\nOpen your posting calendar for the details.` | `Jua Kali Salon`, `Weekend offer`, `Facebook posted, Instagram posted` | `Open calendar` | `jua-kali-salon/publishing` |
| `mf_daily_brief_v1_btn` | `Good morning {{1}},\n\nYour marketing brief for *{{2}}* is ready.\n\nOpen it for the full page and today's suggested actions.` | `Jua Kali Salon`, `Monday 16 November` | `Open brief` | `jua-kali-salon/dashboard` |
| `mf_new_lead_v1_btn` | `Hi {{1}},\n\nA new lead came in from {{2}}: *{{3}}*.\n\nOpen the lead to reply while it is fresh.` | `Jua Kali Salon`, `Facebook lead form`, `Brian O.` | `Open lead` | `jua-kali-salon/leads/5b0e2d7a-1c3f-4e8a-9b6d-2a7c4f1e0d93` |
| `mf_weekly_summary_v1_btn` | `Hi {{1}},\n\nYour marketing week: {{2}} posts went out, {{3}} leads came in and {{4}} AI credits were used.\n\nOpen the full summary in MarketFlow.` | `Jua Kali Salon`, `6`, `14`, `42` | `Open summary` | `jua-kali-salon/dashboard` |

Notes for the owner when submitting:

- `mf_low_credits_btn` is not needed. Low AI credit alerts already go through the central
  `credits_balance_low_v1_btn` template (`billing.balance.low`, one consumer for every credit), so a
  second template would send two alerts for the same balance.
- `mf_new_lead_v1_btn` and `mf_weekly_summary_v1_btn` are written for completeness. No code sends them
  yet: new-lead alerts are email only by the owner's decision of 2026-11-11, and there is no weekly
  summary job yet. Submitting them early does no harm.
- Each of these messages also has an email form (the active channels rule): the daily brief email
  exists; video ready and publish result are shown in the Studio and the posting calendar.
- Sample values contain no real customer data. If Meta answers `INCORRECT_CATEGORY`, remove "ad"
  from the body first (for example "Your video *{{2}}*") and submit it as `_v2`.
