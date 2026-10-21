# WhatsApp for the MarketFlow Ads Assistant (planned, 2026-10-22)

Status: **planned, not built.** Design source: `.claude/plans/marketflow-ads-autopilot-2026-10-22.md`
(sections 4.6, 4.8 and 4.11). Phase 3 of that plan builds it. Every rule in
`whatsapp-template-policy.md` still applies.

The MarketFlow ads assistant lets a business create, review, approve and publish video ads from
WhatsApp. notifications-api owns every WhatsApp send and the inbound webhook, so the WhatsApp parts
of that flow live here. marketflow-ai owns the conversation and marketflow-api owns the ad data.

## What exists today

| Capability | Today | File |
|---|---|---|
| Template send (body params, one URL button suffix, fallback template) | Yes | `internal/providers/whatsapp/metacloud.go` `SendWhatsApp` |
| Interactive `cta_url` (one link button) | Yes | same |
| Plain text | Yes | same |
| Image, video or document send | **No** | |
| Reply buttons and list messages | **No** | |
| Inbound `text` and template quick-reply `button` | Yes | `internal/http/handlers/webhooks.go` `WhatsAppIncoming` |
| Inbound `interactive.button_reply` and `interactive.list_reply` | **No** | |
| `X-Hub-Signature-256` check on the inbound webhook | **No (security gap)** | `webhooks.go` |
| Routing inbound messages to other services | **No** (logged and stored in the tenant inbox only) | `whatsappinbox/service.go` |

## Webhook signature check (done 2026-10-22, needs one config step)

The inbound webhook (`POST /api/v1/webhooks/whatsapp/meta`) now verifies Meta's
`X-Hub-Signature-256` header (`internal/http/handlers/webhooks_signature.go`, wired in
`internal/app/app.go`).

How it works:
- HMAC-SHA256 of the raw body with the Meta app secret, compared in constant time. The body is read once, capped at 1 MB, and the same bytes are checked and parsed.
- The secret comes from the platform `whatsapp/meta_cloud` setting `app_secret` (Platform providers page, stored encrypted; the loader caches for 30 s). The environment variable `WHATSAPP_APP_SECRET` is the fallback.
- A secret is configured: a missing or wrong signature gets 401.
- No secret configured: requests are accepted with a warning at most every 10 minutes, so the deploy cannot cut off live traffic.

Rollout:
1. Deploy.
2. Save the app secret as `app_secret` under the platform's WhatsApp, Meta Cloud provider settings.
3. Confirm the "accepted without signature check" warning stops and genuine deliveries still return 200.
4. Set `WHATSAPP_WEBHOOK_REQUIRE_SIGNATURE=true` so an unset secret rejects instead of accepting.

Tests: `webhooks_signature_test.go` covers valid, wrong, missing and malformed signatures, lenient and strict modes, the env fallback, the stored setting taking priority, the handler's 401/200 responses, and the oversized-body cap.

## Planned sends

1. **Video and image by link.** Cloud API media messages accept `{"video": {"link": "<url>"}}`.
   Meta fetches the file itself, so no video bytes pass through our cluster. Limits: video up to
   16 MB, MP4 with H.264 video and AAC audio. MarketFlow renders a WhatsApp rendition that fits.
   The link is the provider-hosted or public R2 preview URL (see the plan, section 4.6). An optional
   caption carries no raw URL.
2. **Reply buttons**: an interactive `button` message with at most 3 buttons, titles up to 20
   characters, each with a payload id (for example `approve:<version_id>`).
3. **List messages**: an interactive `list` message for choices such as target platforms or
   languages (up to 10 rows).
4. The link policy holds: a link is always a `cta_url` button inside the 24-hour window, or a `_btn`
   template outside it. Preview links use the fixed domain `https://marketflow.codevertexafrica.com/{{1}}`
   with suffix `r/<token>`.

New fields on the send request (metadata, no schema change): `media_type`, `media_link`,
`media_caption`, `reply_buttons` (`[{id,title}]`), `list` (`{button_text, sections:[{title, rows:[{id,title,description}]}]}`).

## Planned inbound routing

1. Parse `interactive.button_reply` and `interactive.list_reply` (id and title) next to `text` and
   `button`.
2. STOP and opt-out handling runs first, unchanged.
3. Messages that belong to a MarketFlow assistant conversation are published as
   `notifications.whatsapp.inbound` through the outbox. Payload: tenant id (resolved from the
   receiving number or the conversation binding), `wa_id`, message id, type, text, button or list
   payload id, `referral` (for click-to-WhatsApp ads, including `ctwa_clid`), timestamp.
4. A conversation is "MarketFlow bound" when the receiving number is the platform assistant number
   or a tenant number with the assistant switched on. Other messages keep today's inbox behaviour.
5. Consumers deduplicate on the WhatsApp message id with the shared `IdempotencyStore`.
6. The first inbound message of a click-to-WhatsApp ad carries a `referral` object with `ctwa_clid`.
   It must be captured on that first message, because later messages don't carry it.

## Planned templates (UTILITY, `_btn`, button domain `https://marketflow.codevertexafrica.com/{{1}}`)

| Name | Purpose | Body variables | Button suffix |
|---|---|---|---|
| `mf_video_ready_v1_btn` | A draft ad is ready for review | business name, ad title | `r/<token>` |
| `mf_publish_result_v1_btn` | An ad was published, or a platform rejected it | ad title, platform, result | `<tenant-slug>/creatives/<id>` |
| `mf_new_lead_v1_btn` | A new lead came from an ad | lead first name, ad or campaign name | `<tenant-slug>/leads/<id>` |
| `mf_weekly_summary_v1_btn` | Weekly results per agent | ads made, leads, spend | `<tenant-slug>/dashboard` |
| `mf_low_credits_v1_btn` | AI credits at 20% or 0% | remaining credits, plan name | `<tenant-slug>/settings/billing` |

Wording follows section 3 of the template policy: no variable at the start or end of the body, no
promotional or credential words, one example per variable. Each needs an email twin
(`feedback_active_channels_email_whatsapp`). Sync with a dry run first, then submit, and switch the
code over only once Meta shows `APPROVED`.

## Billing

- Assistant conversations on the platform number use `senderScope: platform` and are not charged
  to the tenant's WhatsApp quota.
- Tenant-number sends consume the tenant's `whatsapp` credits as today.
- AI turns are charged separately as `ai_credits` by marketflow-ai.

## Open items

- Confirm which number carries the assistant: the existing platform number or a dedicated one.
- Confirm current Meta limits for reply buttons and list rows against the official reference.
