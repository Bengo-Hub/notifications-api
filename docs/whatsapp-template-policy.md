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

The shared apps serve every tenant under its slug, so a tenant on its own domain still gets a
working button: the suffix is the path of its link (`buttonURLSuffix`, `posButtonSuffix`).

## 3. Template formatting (Meta review rules)

- Body variables numbered `{{1}}`..`{{n}}` without gaps, one `example` value each.
- A body never starts or ends with a variable; close with a short sentence.
- Emphasis with `*bold*` on the key value (order number); blank lines between sections.
- `https` button URLs, at most one `{{1}}`, at the end, with one example; button text up to 25
  characters.
- A new wording is a new template name (`_v2`, `_v3`, ...); approved names are never edited in
  place. Deleted names cannot be reused for about four weeks.
- Category `UTILITY` for transactional messages; `AUTHENTICATION` uses Meta's fixed format.

## 4. Publishing

1. Add the template to `templates.json` (button variant for anything with a link).
2. `go test ./internal/whatsapp/... ./cmd/worker/` (manifest lint, parameter counts, link policy).
3. After deploy: notifications-ui Templates > WhatsApp sync, dry run first (or
   `POST /api/v1/platform/whatsapp/templates/sync {"dry_run":true,"only":[...]}` as a platform
   admin), then submit. The results show Meta's review status (`meta_status`).
4. Switch the code to the new name only once it shows `APPROVED`, or accept that WhatsApp is
   skipped for that message until it is.
