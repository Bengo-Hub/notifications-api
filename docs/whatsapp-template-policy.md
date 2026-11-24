# WhatsApp Template and Link Policy

Rules for every WhatsApp message the platform sends, and for every template added to
`internal/whatsapp/templatesync/templates.json`. Tests enforce every rule that can be checked without
Meta: `internal/whatsapp/templatesync/manifest_lint_test.go` (format, button domains, banned words,
words per variable, free text bodies) and `cmd/worker/whatsapp_link_policy_test.go` (no link in a
WhatsApp text body).

## 1. Links are buttons, never text

A WhatsApp message never shows a raw URL. A link always goes out as a tappable button, like
"Track Order" or "View Order" under the message.

| Send type | How the link is attached |
|---|---|
| Template (business-initiated, any time) | A template with a `URL` button whose URL is a fixed domain plus a `{{1}}` suffix (`_btn` templates). The code passes the suffix as `template_button_param`. |
| Free-form (inside the customer's 24h reply window) | An interactive `cta_url` message: `cta_button_text` + `cta_button_url` metadata. The worker adds this automatically from the message's link field (`order_link`, `track_link`, `review_link`, `action_link`, `download_link`, ...; see `cmd/worker/whatsapp_link_policy.go`). |

Consequences:

- Template bodies have no link variable. The button carries it.
- WhatsApp text bodies under `templates/whatsapp/**/*.txt` never include a link: no link variable
  (`{{ .action_link }}`, `..._url`), no `http(s)://` and no bare domain such as `www.x` or
  `ordering.codevertexafrica.com/...`, at any folder depth.
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
| Credit balance and billing (the tenant's own credits) | `https://pricing.codevertexafrica.com/{{1}}` | `billing` |
| Provider balance (platform owner only) | `https://marketflow.codevertexafrica.com/{{1}}` | `api/provider-billing/<provider>`, which redirects to that provider's own billing page (the platform pays the provider directly; the page is `billing_url` on the provider card in MarketFlow, with a default per provider); `platform/providers` for a provider name that is not a plain id |
| Public ticket page (a customer's own support request) | `https://ticketing.codevertexafrica.com/{{1}}` | `support/track?org=<tenant-slug>&ticket=<ticket number>`, built from the tenant's slug and the event's `ticket_number` (`ticketTrackSuffix`); ticketing-api's events carry no link or token for it |

A button on any other domain fails the manifest lint: a new domain is a policy decision, so its row
goes here first and into `buttonDomains` in `manifest_lint_test.go`. The pricing and MarketFlow rows
were missing from this table until the lint was added on 17 November 2026; their templates
(`credits_*_btn`, `provider_balance_low_v1_btn`) were already approved on those domains, and
`provider_balance_low_v3_btn` keeps the MarketFlow one. A tenant's own credit alert keeps its pricing button; only the platform owner's provider alert forwards to a provider's site, and only through that redirect, never as a link in the body.

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

## 2c. Ticket replies on WhatsApp

An agent's reply on a WhatsApp ticket goes by email only, by the owner's decision of 17 November 2026:
`TICKETING_WHATSAPP_REPLIES` is false. With it on (`cmd/worker/ticketing_whatsapp.go`), the reply also
goes to the customer from the tenant's own number:

- inside the customer's 24-hour window, the reply itself as free-form text (`ticketing/ticket_reply`);
- after the window, the `ticketing_ticket_reply_v1_btn` template: a notice that the business's support
  team posted a new reply on request *TKT-n*, with a "View reply" button to the public ticket page. The
  reply text is never a variable, since a body that is mostly free text is marketing to Meta. Without the
  tenant's slug or the ticket number there is no button, so nothing goes on WhatsApp. The template has no
  fallback: until Meta approves it on the tenant's WABA the send fails and WhatsApp is skipped for that
  reply, as for every `_btn` template; the email copy goes either way.

| Template | Body | Example values | Button |
|---|---|---|---|
| `ticketing_ticket_reply_v1_btn` (UTILITY, en_US) | `Hi {{1}},\n\nThe support team at {{2}} has posted a new reply on your support request *{{3}}*.\n\nOpen the request to read the reply.` | `Amina`, `Urban Loft Cafe`, `TKT-000042` | "View reply", `https://ticketing.codevertexafrica.com/{{1}}`, example `support/track?org=urban-loft&ticket=TKT-000042` |

The template reaches a tenant's WABA through the `ticketing` entry of
`internal/whatsapp/tenanttemplates/selection.json`.

## 3. Template formatting (Meta review rules)

- Body variables numbered `{{1}}`..`{{n}}` without gaps, one `example` value each.
- A body never starts or ends with a variable; close with a short sentence.
- Enough words around the variables: Meta rejects a body with too few as error 2388293 ("parameters words
  ratio"). Three variables and six other words was refused. Write at least four words per variable; the
  lint counts every word of the body that is not a variable and fails a new template below that. The 37
  templates Meta approved before the lint (`shortRatioApproved` in `manifest_lint_test.go`) are held only
  to more than two words per variable, Meta's own refusal level: rewriting a working template just for
  the ratio would put it back into review for nothing. No name is ever added to that list, and a name
  leaves it when its template is rewritten.
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
  The lint checks these words in every UTILITY body, example value and button text, in any case and with
  any ending ("enjoyed", "logins"), and "sign-in" too.
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
  or "stay connected", no thanks for loyalty, and no button that sells. A button names what its page
  shows ("View Order", "View balance", "Service details"), never what we would like the reader to do
  there. The lint fails a UTILITY button whose text has "Top up", "Buy", "Review us", "Rate us",
  "Leave Feedback", "Visit our website" or "Shop now" (`sellingButtonText`); its allowlist
  (`sellingButtonAllowed`) is empty, since a template that needs such a button is a MARKETING one.
- A body that is mostly one free text variable is marketing to Meta whatever its wording. The lint fails
  a template whose longest example has at least as many words as the rest of its body, unless it is
  billed as MARKETING (`category` or `meta_category`). `broadcast_update_v1` (MARKETING) and
  `broadcast_service_notice_v1` (`meta_category` MARKETING) pass that way, by design.
- `category` in `templates.json` is what we submit. When Meta holds a template under another category,
  record it as `meta_category`: credits bill that (`TemplateDef.Billed`) and the audits stop flagging it.
- Check Meta's verdict after every sync and once a month: `GET /api/v1/platform/whatsapp/templates/categories?recategorized=true`
  (platform owner) lists every template on every WABA we send from whose Meta category differs from ours.
  The tenant's own view (`GET /api/v1/settings/whatsapp/templates`) shows `meta_category` and `recategorized` too.
- A rewrite under a new name can ship before Meta approves it if `internal/providers/whatsapp/superseded.go`
  maps it to the approved template it replaces (with the parameter order of the old one): until approval
  every send falls back to the old template. Drop the entry once the rewrite shows APPROVED as UTILITY.
  When the code also names a fallback that is itself a rewrite in review (the order review template
  falls back to the one-button order template), the superseded entry wins, with every button value of
  the send (`template_fallback_button_params`).

The audit of 17 November 2026 ran the new lint rules over every template. Rewrites for the approved
templates with a banned word, each kept to the same parameters and buttons:

| Old template | Problem | Replacement | Code |
|---|---|---|---|
| `ordering_order_delivered_v3_btn`, `ordering_order_delivered_v4_btn` | "We hope you enjoy it", "Thank you for choosing us" | `ordering_order_delivered_v7_btn`, `ordering_order_delivered_v8_btn`: "Your order *X* has been delivered and is now closed. The order page shows its full details." (v5 and v6 were the first names, see below) | sent now, falling back to v3 and v4 (`superseded.go`) |
| `ordering_order_completed_v1_btn`, `ordering_order_completed_v2_btn` | the same | `ordering_order_completed_v5_btn`, `ordering_order_completed_v6_btn` (v3 and v4 were the first names) | sent now, falling back to v1 and v2 |
| `projects_client_invited_v1_btn` | "Sign in", and "has invited you" (the wording Meta refused for Hadia's member invite) | `projects_client_invited_v2_btn`: "X has given you access to the project *Y* in its client portal" | `cmd/worker/projects_portal.go` sends v2; approved at Meta on 17 November 2026, so its `superseded.go` fallback to v1 is gone |
| `logistics_delivery_completed_btn` | "Thank you for choosing us" | `logistics_delivery_completed_v4_btn` (v2 and v3 were earlier names) | no code sends either |
| `hrm_leave_approved` | "Enjoy your time off", and under four words per variable | `hrm_leave_approved_v2` | no code sends either |

The same day the owner decided that a button inviting the reader to review, visit or buy goes the same
way (section 3b). Each rewrite keeps the body, the parameters and the button URL with its suffix, and
only the button text changes to say what the page shows:

| Old template | Problem | Replacement | Code |
|---|---|---|---|
| `ordering_order_delivered_v3_btn` (and its first rewrite `ordering_order_delivered_v5_btn`, never approved) | "Leave Feedback" button | `ordering_order_delivered_v7_btn`: "View Order" | `cmd/worker/order_consumer.go` sends it, falling back to v3 |
| `ordering_order_delivered_v4_btn` (and `ordering_order_delivered_v6_btn`) | "Leave Feedback" and "Review us on Google" buttons | `ordering_order_delivered_v8_btn`: "View Order" and "View on Google" (the storefront's `<slug>/review/google`, which forwards to the business's Google page) | sent when the business set a Google review link, falling back to v4 with both buttons |
| `ordering_order_completed_v1_btn` (and `ordering_order_completed_v3_btn`) | "Leave Feedback" button | `ordering_order_completed_v5_btn`: "View Order" | sent, falling back to v1 |
| `ordering_order_completed_v2_btn` (and `ordering_order_completed_v4_btn`) | "Leave Feedback" and "Review us on Google" buttons | `ordering_order_completed_v6_btn`: "View Order" and "View on Google" | sent with a Google review link, falling back to v2 |
| `codevertex_enquiry_received_v1_btn` | "Visit our website" button | `codevertex_enquiry_received_v2_btn`: "Service details" (the `services` page) | `cmd/worker/digitika_consumer.go` sends it, falling back to v1 |
| `provider_balance_low_v1_btn` | "Top up" button, and "Top it up from the providers page" in the body | `provider_balance_low_v2_btn`: "View balance"; the body ends "The providers page shows each provider's balance." | `cmd/worker/balance_low_consumer.go` sends it, falling back to v1 |
| `logistics_delivery_completed_v3_btn` | "Leave Feedback" button | `logistics_delivery_completed_v4_btn`: "View Order" | no code sends either |

The first order rewrites (delivered v5 and v6, completed v3 and v4) left the manifest without being
approved, so their rewrites fall back straight to the approved names. If they were already submitted,
they stay on the WABAs unused; deleting them in WhatsApp Manager is optional.

`ispbilling_expiry_reminder_v2` had three and a half words per variable. It has not been submitted yet
(section 4), so its body was lengthened in place.

## 4. Publishing

1. Add the template to `templates.json` (button variant for anything with a link).
2. `go test ./internal/whatsapp/... ./cmd/worker/` (manifest lint, parameter counts, link policy).
3. After deploy: notifications-ui Templates > WhatsApp sync, dry run first (or
   `POST /api/v1/platform/whatsapp/templates/sync {"dry_run":true,"only":[...]}` as a platform
   admin), then submit. The results show Meta's review status (`meta_status`). Add `"tenant_id"` to submit
   to the WABA of that tenant's number instead of the platform's (the shared portfolio WABA, while the
   platform number has no WABA of its own); `names` then picks templates whatever that tenant's usage selects.
4. Switch the code to the new name only once it shows `APPROVED`, or accept that WhatsApp is
   skipped for that message until it is.

Templates waiting for a Meta sync after their deploy:

| Template | Sent for |
|---|---|
| `ispbilling_expiry_reminder_v2`, `ispbilling_subscription_expiring_v2` | not sent yet (ISP notices go by SMS and email) |
| `ticketing_ticket_reply_v1_btn` | an agent reply after the customer's WhatsApp window, only with `TICKETING_WHATSAPP_REPLIES` on (section 2c); sync it to each ticketing tenant's WABA |
| `ordering_order_delivered_v7_btn`, `ordering_order_delivered_v8_btn`, `ordering_order_completed_v5_btn`, `ordering_order_completed_v6_btn` | delivered and completed orders (sent now through the `superseded.go` fallback); platform WABA and each ordering tenant's WABA |
| `codevertex_enquiry_received_v2_btn` | the reply to a website enquiry (sent now through the fallback to v1); platform WABA |
| `provider_balance_low_v3_btn` | a MarketFlow provider balance alert to the platform owner (sent now through the fallback to v1, whose button on the same domain takes the same redirect suffix); platform WABA. It replaces `provider_balance_low_v2_btn`, never approved, whose body pointed at the MarketFlow providers page; if v2 was submitted it can stay on the WABA unused |
| `logistics_delivery_completed_v4_btn`, `hrm_leave_approved_v2` | nothing yet (no code sends them) |

Approved as UTILITY on 16 November 2026 within the hour, with their `superseded.go` fallbacks removed:
`notifications_test_message_v2`, `credits_balance_low_v2_btn`, `hadia_registry_published_v2_btn`,
`hadia_change_requested_v2_btn`, `hadia_legal_updated_v2_btn`, `pos_loyalty_referral_v2` and the three
`projects_task_*` templates.

Approved on 17 November 2026, with its `superseded.go` fallback removed: `projects_client_invited_v2_btn`
(a client given access to a project's portal, sent by `cmd/worker/projects_portal.go`).

## 4b. What Meta tells us after a template is live

Meta keeps judging a template after approval. Two webhook fields report it, and both must be ticked on the
Meta app (WhatsApp, Configuration, Webhook fields) next to `messages`:

- `template_category_update`: Meta moved a template to another category (or will). The new category is
  recorded at once (`internal/whatsapp/templatecat`), credits bill sends by it from the next minute, and ops
  get an alert. A move to Marketing goes by email and Slack with the cost, what Meta does to delivery
  (131049) and the fix; a move to a cheaper category is Slack news only.
- `message_template_status_update`: Meta approved, rejected, paused or disabled a template. Approvals are
  Slack news only; the rest go by email and Slack with Meta's reason.

Ops WhatsApp is never used for these alerts. A sweep re-reads every WABA's categories every 6 hours and
alerts on moves the webhook missed (one replica per interval).

Meta's later verdict on each send is recorded too: the delivery log row moves to delivered, read or failed
with Meta's error code and reason (Monitoring no longer shows "sent" for a message Meta refused), the Test
button waits up to six seconds and says what Meta did with its message, and a number Meta cannot deliver
to (131026) is skipped on WhatsApp for that sender for a week, with its backup number used when there is one.

## 5. MarketFlow assistant templates (Sprint 17)

MarketFlow's WhatsApp assistant writes to business owners from the platform number. Inside the
24 hour window it sends free-form messages with reply buttons. Outside the window it needs the
templates below. On 2026-11-20 the Sync dialog showed all 150 manifest templates approved and none
of these on the WABA, so the five that code sends are now in `templates.json`: Templates > Sync
WhatsApp to Meta submits them on the platform WABA (Codevertex). `mf_new_lead_v1_btn` stays out
until code sends it.

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
| `mf_integration_reconnect_v1_btn` | `Hi {{1}},\n\nMarketFlow can no longer reach {{2}}, so posts and ads on it are paused.\n\nOpen your integrations page to connect it again.` | `Jua Kali Salon`, `the Facebook Page Jua Kali Salon` | `Reconnect` | `jua-kali-salon/settings/integrations` |
| `mf_testimonial_received_v1_btn` | `Hi {{1}},\n\n{{2}} sent your business a new testimonial through your testimonial link. It is not shown anywhere until you approve it.\n\nOpen it to approve or decline.` | `Jua Kali Salon`, `Amina` | `Review testimonial` | `jua-kali-salon/settings/testimonials?review=3c1f8e2a-5b7d-4f0a-9e6c-1d2b3a4c5e6f` |

`mf_testimonial_received_v1_btn` (2026-11-24) goes to the business's own WhatsApp number when a
customer sends a testimonial through the business's public link. It reports a fact about the
business's account (a testimonial waits for its review) and invites nothing, so it is UTILITY. The
customer's words are never a variable; the business reads them in MarketFlow. Testimonials MarketFlow
finds on the business's website are told by email only, once per weekly read.

Notes for the owner when submitting:

- `mf_low_credits_btn` is not needed. Low AI credit alerts already go through the central
  `credits_balance_low_v2_btn` template (`billing.balance.low`, one consumer for every credit), so a
  second template would send two alerts for the same balance.
- `mf_new_lead_v1_btn` and `mf_weekly_summary_v1_btn` are written for completeness. No code sends them
  yet: new-lead alerts are email only by the owner's decision of 2026-11-11, and there is no weekly
  summary job yet. Submitting them early does no harm.
- Each of these messages also has an email form (the active channels rule): the daily brief email
  exists; video ready and publish result are shown in the Studio and the posting calendar.
- Sample values contain no real customer data. If Meta answers `INCORRECT_CATEGORY`, remove "ad"
  from the body first (for example "Your video *{{2}}*") and submit it as `_v2`.

## 5b. MarketFlow lead follow-up (tenant's own number)

Every MarketFlow tenant answers its new leads automatically (owner decision of 24 November 2026): an
email at once, the WhatsApp message below when the lead ticked the WhatsApp consent box, and follow-up
emails after a day and after three days unless the lead replies, books a call or the team contacts
them. Unlike the `mf_` templates in section 5, this one goes from the **tenant's own number** to the
tenant's own lead and names the tenant, never Codevertex. It reaches a tenant's WABA through the
`marketflow` entry of `internal/whatsapp/tenanttemplates/selection.json` (prefix `mf_lead_`).

| Template | Body | Example values | Button |
|---|---|---|---|
| `mf_lead_hello_v1_btn` (UTILITY, en_US) | `Hi {{1}},\n\nThank you for your enquiry to {{2}}. We have received it and a member of the team will contact you soon.\n\nYou can also pick a time for a call with the button below.` | `Esther`, `Jua Kali Salon` | "Book a call", `https://marketflow.codevertexafrica.com/{{1}}`, example `book/jua-kali-salon` |

`{{1}}` is the lead's first name ("there" when the lead gave none), `{{2}}` the business name. The
button opens the business's own booking page (`book/<tenant-slug>`), on the MarketFlow domain already
in the button domain table. The message confirms the person's own enquiry and offers nothing, so it is
submitted as UTILITY. Until Meta approves it on a tenant's WABA, or when the tenant has no WhatsApp
number of its own, the send fails and MarketFlow skips the WhatsApp step; the email steps still go.
