package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/moneyfmt"
)

// Hadia (gift registry, one client tenant) notifications. hadia-api publishes hadia.<event_type>
// through its outbox (hadia-api docs/architecture.md, "Published by hadia-api"). One event can reach
// more than one person, such as a reservation that goes to the guest and to the owner, so each event
// maps to a list of messages. Every link is a WhatsApp URL button on hadiafrica.com, never body text
// (docs/whatsapp-template-policy.md). Hadia is a single tenant on its own domain, so button paths
// carry no tenant slug.

// hadiaButtonBase is the domain fixed in every hadia_*_btn template.
const hadiaButtonBase = "https://hadiafrica.com"

// hadiaTrustNote closes every Hadia email (shared/base.html, brand_note). Complaints about
// look-alike fundraising sites made naming the one real address a standing rule (Hadia Sprint 08).
const hadiaTrustNote = "Hadia's messages and links only ever use hadiafrica.com, and gifts are paid only to M-Pesa paybill 756756 with the account number shown on hadiafrica.com. Hadia will never ask for your M-Pesa PIN or a one-time code, never asks you to pay a different number, and never asks for money to release a gift, a payout or a refund. If anyone does, it is a scam: do not pay, and report it at hadiafrica.com/help."

// hadiaMessage is one message an event sends. It reuses maskaniMapping's fields (template,
// recipient keys, data, WhatsApp template and parameters); PathKey names the payload field holding
// the full hadiafrica.com link the button and email open.
type hadiaMessage struct {
	maskaniMapping
	PathKey string
	// FixedPath is the button suffix of a message whose payload carries no link.
	FixedPath string
	// WhatsAppIf limits a message to email unless it holds; nil always allows WhatsApp.
	WhatsAppIf func(p map[string]any) bool
	// Digest marks the owner alert an owner may take as one daily summary instead (N3): with
	// owner_digest "daily" on the payload it is held for hadia_digest.go rather than sent.
	Digest bool
}

// hadiaCaseLabel turns a case kind into words for staff ("manual_refund" reads "manual refund").
func hadiaCaseLabel(kind string) string {
	if kind == "" {
		return "exception"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

// hadiaPath turns a full hadiafrica.com link into the button suffix ("r/AB12CD34/reserved/tok").
// A link on any other host gives no button, so a payload can never point a button elsewhere.
func hadiaPath(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" {
		return ""
	}
	base, _ := url.Parse(hadiaButtonBase)
	host := strings.ToLower(u.Host)
	// Hadia's own subdomains count too (staging.hadiafrica.com): only the path is kept, so the
	// button still lands on the fixed template domain and can never point elsewhere.
	if host != base.Host && !strings.HasSuffix(host, "."+base.Host) {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

// hadiaEmailBase is where email links open: the tenant's Hadia app URL from auth-api (service_urls
// "hadia", then app_url), so staging mail opens staging. WhatsApp buttons keep hadiaButtonBase, the
// domain Meta approved in each template.
func hadiaEmailBase(ti *tenantInfo) string {
	fallback := hadiaButtonBase
	if ti != nil && ti.AppURL != "" {
		fallback = ti.AppURL
	}
	return ti.ServiceURL("hadia", "", fallback)
}

func hadiaGuestData(p map[string]any, _ *tenantInfo) map[string]any {
	return map[string]any{"name": waParam(p["guest_name"], "there"), "item": mStr(p, "item_title"),
		"registry": mStr(p, "registry_title"), "expires_at": mTime(p, "expires_at"), "owner": waParam(p["owner_name"], "the couple")}
}

// hadiaFundData is the owner's view of a fund or gift crossing a step or its target.
func hadiaFundData(p map[string]any, _ *tenantInfo) map[string]any {
	early, _ := p["early"].(bool)
	return map[string]any{"name": waParam(p["owner_name"], "there"), "item": mStr(p, "item_title"),
		"registry": mStr(p, "registry_title"), "raised": mMoney(p, "raised"), "target": mMoney(p, "target"),
		"percent": mPercent(p), "over_by": mMoneyOr(p, "over_by", ""), "early": early, "cash_fund": mStr(p, "kind") == "cash_fund"}
}

// hadiaChangeWords words a co-owner's requested change for the owner.
func hadiaChangeWords(kind string) string {
	switch kind {
	case "publish":
		return "publish the registry"
	case "unpublish":
		return "unpublish the registry"
	case "visibility":
		return "change who can see the registry"
	case "rotate_link":
		return "make a new share link"
	}
	return "change the registry"
}

// mInt reads a whole count from the payload (JSON numbers arrive as float64); 0 when absent.
func mInt(p map[string]any, key string) int {
	switch v := p[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// hadiaMoneyIn formats an amount in the payload's own currency ("KES 6,450"), KES when the payload
// names none; "" when the amount is absent.
func hadiaMoneyIn(p map[string]any, key string) string {
	f, err := strconv.ParseFloat(mStr(p, key), 64)
	if err != nil {
		return mStr(p, key)
	}
	currency := mStr(p, "currency")
	if currency == "" {
		currency = "KES"
	}
	return moneyfmt.Format(f, currency)
}

// mPercent words a whole per cent from the payload ("75%"); "" when absent.
func mPercent(p map[string]any) string {
	switch v := p["percent"].(type) {
	case float64:
		return fmt.Sprintf("%d%%", int(v))
	case int:
		return fmt.Sprintf("%d%%", v)
	case int64:
		return fmt.Sprintf("%d%%", v)
	}
	return ""
}

// hadiaPayoutFailure is one of the owner's messages about a payout that did not go through, sent for
// the reasons it is true of. A failure before treasury answered has no net yet, so the gross is shown.
func hadiaPayoutFailure(template, subject, waTemplate string, sentFor func(reason string) bool) hadiaMessage {
	return hadiaMessage{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: template, PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return subject },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			amount := "net"
			if !mPositive(p, "net") {
				amount = "gross"
			}
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"), "amount": mMoney(p, amount)}
		},
		WATemplate: waTemplate,
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "amount", "registry") },
		Skip:       func(p map[string]any) bool { return !sentFor(mStr(p, "reason")) },
	}}
}

var hadiaMappings = map[string][]hadiaMessage{
	// A one-time code for a guest (reserving with a phone) or an owner (payout number). The shared
	// auth OTP templates from the platform number, as sign-in codes, so the tenant's own plan never
	// blocks a code.
	"otp.requested": {{maskaniMapping: maskaniMapping{
		TemplateID: "auth/otp_verification", WATemplateID: "auth/otp", PhoneKey: "phone", EmailKey: "email",
		Subject: func(map[string]any) string { return "Your Hadia verification code" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": "there", "otp": mStr(p, "otp"), "ttl_minutes": 5, "brand_name": "Hadia"}
		},
		WATemplate: "auth_otp",
		WAParams:   func(d map[string]any) []string { return mParams(d, "otp") },
		WAButton:   func(d map[string]any) string { return fmt.Sprint(d["otp"]) },
		Platform:   true,
	}}},
	// The owner's registry is live: the share message.
	"registry.published": {{PathKey: "share_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/registry_published", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Your Hadia registry is live" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "title")}
		},
		WATemplate: "hadia_registry_published_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "registry") },
	}}},
	// A guest reserved a gift: their manage link, and an alert to the owner.
	"reservation.created": {
		{PathKey: "manage_url", maskaniMapping: maskaniMapping{
			TemplateID: "hadia/reservation_created", PhoneKey: "guest_phone", EmailKey: "guest_email",
			Subject:    func(map[string]any) string { return "Your gift reservation" },
			Data:       hadiaGuestData,
			WATemplate: "hadia_reservation_created_v1_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "item", "registry", "expires_at") },
		}},
		{PathKey: "owner_url", maskaniMapping: maskaniMapping{
			TemplateID: "hadia/reservation_owner_alert", PhoneKey: "owner_phone", EmailKey: "owner_email",
			Subject: func(map[string]any) string { return "A guest reserved a gift" },
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				// Guest names reach the owner only when the registry shows them (show_reserver_names).
				guest := "A guest"
				if b, _ := p["show_reserver_names"].(bool); b {
					guest = waParam(p["guest_name"], "A guest")
				}
				return map[string]any{"name": waParam(p["owner_name"], "there"), "guest": guest, "item": mStr(p, "item_title")}
			},
			WATemplate: "hadia_reservation_owner_alert_v2_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "guest", "item") },
			Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
		}},
	},
	// A legal document changed materially (hadia-api legal notices, 8 November 2026): one event per
	// owner. hadia-api sends no phone yet, so it goes by email; the WhatsApp template is declared for
	// when Meta approves it.
	// A new staff member's way in: an email only, with no password in it (they set their own).
	// FR-41: the weekly link check found the shop's price changed or the page gone. Email only, so no
	// WhatsApp template waits on Meta; the owner decides, nothing on the registry changed by itself.
	// Owner progress alerts (owner, 9 November 2026). Email only for now: Hadia has no WhatsApp
	// number of its own yet, and no approved template carries these figures.
	"fund.milestone": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/fund_milestone", EmailKey: "owner_email",
		Subject: func(p map[string]any) string {
			return fmt.Sprintf("%s is %s funded", mStr(p, "item_title"), mPercent(p))
		},
		Data: hadiaFundData,
		Skip: func(p map[string]any) bool { return mStr(p, "owner_email") == "" },
	}}},
	"fund.over_target": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/fund_over_target", EmailKey: "owner_email",
		Subject: func(p map[string]any) string {
			return fmt.Sprintf("%s has passed its target", mStr(p, "item_title"))
		},
		Data: hadiaFundData,
		Skip: func(p map[string]any) bool { return mStr(p, "owner_email") == "" },
	}}},
	"registry.funds_behind": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/funds_behind", EmailKey: "owner_email",
		Subject: func(p map[string]any) string {
			if mStr(p, "kind") == "passed" {
				return "Your event has passed: how your gifts ended"
			}
			return "Your event is coming up: some gifts still need support"
		},
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			var funds []map[string]any
			if list, ok := p["funds"].([]any); ok {
				for _, f := range list {
					if m, ok := f.(map[string]any); ok {
						funds = append(funds, map[string]any{"title": mStr(m, "title"), "raised": mMoney(m, "raised"),
							"target": mMoney(m, "target"), "percent": mPercent(m)})
					}
				}
			}
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"),
				"kind": mStr(p, "kind"), "event_date": mStr(p, "event_date"), "payout_from": mTime(p, "payout_from"), "funds": funds}
		},
		Skip: func(p map[string]any) bool { return mStr(p, "owner_email") == "" },
	}}},
	// A week after the event, guests still to thank (owner decision, 10 November 2026: email and
	// WhatsApp, on by default; hadia-api only sends it while the owner's switch is on). WhatsApp
	// goes once Meta approves the template and Hadia's number is live; email until then.
	"thankyou.reminder": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/thankyou_reminder", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(p map[string]any) string {
			if mInt(p, "unthanked") == 1 {
				return "One guest is waiting for your thank-you"
			}
			return fmt.Sprintf("%d guests are waiting for your thank-you", mInt(p, "unthanked"))
		},
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"),
				"count": fmt.Sprintf("%d", mInt(p, "unthanked")), "one": mInt(p, "unthanked") == 1}
		},
		WATemplate: "hadia_thankyou_reminder_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "count", "registry") },
		Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
	}}},
	// A registry owner invited a co-owner (FR-04, 10 November 2026). WhatsApp only: the invitee has no
	// account or email yet. Until Meta approves the template and Hadia's number is live nothing goes,
	// and hadia-ui lets the owner share the invitation themselves.
	"registry.member_invited": {{PathKey: "sign_in_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/member_invited", PhoneKey: "invitee_phone",
		Subject: func(p map[string]any) string { return "You are invited to help run a gift registry" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"inviter": waParam(p["inviter_name"], "A Hadia host"), "registry": mStr(p, "registry_title")}
		},
		// v1 was refused as INCORRECT_CATEGORY (14 November 2026); v2 reads as a co-owner notice.
		WATemplate: "hadia_member_invited_v2_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "inviter", "registry") },
		Skip:       func(p map[string]any) bool { return mStr(p, "invitee_phone") == "" },
	}}},
	// A co-owner asks the owner to publish, unpublish, change visibility or make a new share link (owner
	// decision, 10 November 2026: those need the original owner's approval). Email now, WhatsApp once
	// Meta approves the template and Hadia's number is live.
	"registry.change_requested": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/change_requested", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(p map[string]any) string { return "A co-owner is waiting for your approval" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "co_owner": waParam(p["co_owner_name"], "A co-owner"),
				"registry": mStr(p, "registry_title"), "change": hadiaChangeWords(mStr(p, "kind"))}
		},
		WATemplate: "hadia_change_requested_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "co_owner", "change", "registry") },
		Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
	}}},
	"item.link_changed": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/item_link_changed", EmailKey: "owner_email",
		Subject: func(p map[string]any) string {
			if mStr(p, "link_status") == "unavailable" {
				return "A shop link on your registry no longer works"
			}
			return "A shop changed the price of a gift on your registry"
		},
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "item": mStr(p, "item_title"),
				"registry": mStr(p, "registry_title"), "store": waParam(p["store_name"], "the shop"),
				"gone": mStr(p, "link_status") == "unavailable", "price_seen": mMoney(p, "price_seen"),
				"price_now": mMoney(p, "price_now"), "source_url": mStr(p, "source_url")}
		},
		Skip: func(p map[string]any) bool { return mStr(p, "owner_email") == "" },
	}}},
	"staff.invited": {{PathKey: "url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/staff_invited", EmailKey: "email",
		Subject: func(map[string]any) string { return "You have been added to the Hadia team" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there")}
		},
		Skip: func(p map[string]any) bool { return mStr(p, "email") == "" },
	}}},
	"legal.updated": {{PathKey: "url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/legal_updated", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(p map[string]any) string {
			return "We have updated our " + strings.ToLower(waParam(p["title"], "terms"))
		},
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "title": waParam(p["title"], "terms"),
				"summary": mStr(p, "summary"), "effective": mStr(p, "effective_at")}
		},
		WATemplate: "hadia_legal_updated_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "title") },
		Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
	}}},
	// A guest marked a gift bought: the owner is asked to confirm it arrived on the gift log.
	"reservation.purchased": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/reservation_purchased_owner", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "A gift is on its way" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "guest": waParam(p["guest_name"], "A guest"),
				"item": mStr(p, "item_title"), "registry": mStr(p, "registry_title")}
		},
		WATemplate: "hadia_reservation_purchased_owner_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "guest", "item") },
		Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
	}}},
	// Two days before a hold ends.
	"reservation.reminder": {{PathKey: "manage_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/reservation_reminder", PhoneKey: "guest_phone", EmailKey: "guest_email",
		Subject: func(p map[string]any) string {
			if b, _ := p["last_day"].(bool); b {
				return "Last day: your gift reservation ends today"
			}
			return "Your gift reservation ends soon"
		},
		Data:       hadiaGuestData,
		WATemplate: "hadia_reservation_reminder_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "item", "expires_at") },
	}}},
	// The owner's payout number changed: alert the OLD number and the owner's email, so a hijacked
	// session that swaps the number is noticed (Sprint 02 security review). No link. A first number
	// has no old number to alert, so only the email goes.
	"payout_destination.changed": {{maskaniMapping: maskaniMapping{
		TemplateID: "hadia/payout_destination_changed", PhoneKey: "old_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Your Hadia payout number was changed" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "old_number": waParam(p["old_phone_masked"], "your previous number"),
				"new_number": mStr(p, "new_phone_masked"), "hold_until": mTime(p, "hold_until")}
		},
		WATemplate: "hadia_payout_destination_changed_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "old_number", "new_number", "hold_until") },
	}}},
	// A gift arrived (hadia-api, from treasury.escrow.pot_credited): the guest's receipt when their
	// phone is known (guests give no email), and an alert to the owner, or for an owner who chose a
	// daily summary (owner_digest "daily") the gift is held for the 18:00 summary. The receipt is
	// never held.
	"gift.received": {
		{PathKey: "registry_url", maskaniMapping: maskaniMapping{
			TemplateID: "hadia/gift_receipt", PhoneKey: "guest_phone",
			Subject: func(map[string]any) string { return "Your Hadia gift receipt" },
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				return map[string]any{"name": waParam(p["guest_name"], "there"), "registry": mStr(p, "registry_title"),
					"gift": mMoney(p, "amount"), "fee": mMoneyOr(p, "fee", "KES 0"), "total": mMoneyOr(p, "total", mMoney(p, "amount")),
					"receipt": waParam(p["receipt_ref"], "see your payment message")}
			},
			WATemplate: "hadia_gift_receipt_v1_btn",
			WAParams: func(d map[string]any) []string {
				return mParams(d, "name", "registry", "gift", "fee", "total", "receipt")
			},
			Skip: func(p map[string]any) bool { return mStr(p, "guest_phone") == "" },
		}},
		{PathKey: "owner_url", Digest: true, maskaniMapping: maskaniMapping{
			TemplateID: "hadia/gift_owner_alert", PhoneKey: "owner_phone", EmailKey: "owner_email",
			Subject: func(map[string]any) string { return "You received a gift on Hadia" },
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				return map[string]any{"name": waParam(p["owner_name"], "there"), "guest": waParam(p["guest_name"], "A guest"),
					"gift": mMoney(p, "amount"), "registry": mStr(p, "registry_title")}
			},
			WATemplate: "hadia_gift_owner_alert_v1_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "guest", "gift", "registry") },
			Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
		}},
	},
	// A payout reached the owner's number (from treasury.escrow.dispatched).
	"payout.completed": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/payout_completed", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Your Hadia payout was sent" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			d := map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"),
				"gross": mMoney(p, "gross"), "commission": mMoneyOr(p, "commission", "KES 0"), "net": mMoney(p, "net"),
				"reference": waParam(p["reference"], "-")}
			// A refused part stopped a split payout: the rest went back to the registry.
			if mPositive(p, "returned") {
				d["returned"] = mMoney(p, "returned")
			}
			return d
		},
		WAVariant: func(d map[string]any) (string, []string) {
			if _, short := d["returned"]; short {
				return "hadia_payout_completed_part_v1_btn", mParams(d, "name", "net", "registry", "reference", "returned")
			}
			return "hadia_payout_completed_v1_btn", mParams(d, "name", "net", "registry", "reference")
		},
	}}},
	// A payout that did not go through. Each reason gets its own truthful message; the provider's own
	// words stay out because they are technical and Hadia staff act on them. An unconfirmed payout must
	// never say the money is safe: it may have gone.
	"payout.delayed": {
		hadiaPayoutFailure("hadia/payout_delayed", "Your Hadia payout did not go through", "hadia_payout_delayed_v2_btn", func(r string) bool { return r != "approval_rejected" && r != "payout_refused" && r != "not_confirmed" }),
		hadiaPayoutFailure("hadia/payout_declined", "Your Hadia payout was not made", "hadia_payout_declined_v1_btn", func(r string) bool { return r == "approval_rejected" || r == "payout_refused" }),
		hadiaPayoutFailure("hadia/payout_unconfirmed", "We are checking your Hadia payout", "hadia_payout_unconfirmed_v1_btn", func(r string) bool { return r == "not_confirmed" }),
	},
	// One part of a payout split across provider limits.
	"payout.part_paid": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/payout_part_paid", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Part of your Hadia payout was paid" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"),
				"part": mStr(p, "part"), "parts": mStr(p, "parts"), "net": mMoney(p, "net"), "reference": waParam(p["reference"], "-")}
		},
		WATemplate: "hadia_payout_part_paid_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "part", "parts", "registry", "net") },
	}}},
	// A registry closed at expiry with money still owed and its owner has no verified payout number
	// past its hold (hadia-api registry/expiry_payout.go, once per registry). The button opens the
	// registry's payouts page. The same close opens a high priority case for support and finance,
	// which case.opened below already sends to Hadia's staff, so nothing more goes to staff here.
	"payout.number_needed": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/payout_number_needed", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Add a payout number to receive your Hadia gifts" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": waParam(p["registry_title"], "Your registry"),
				"amount": hadiaMoneyIn(p, "amount")}
		},
		WATemplate: "hadia_payout_number_needed_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "registry", "amount") },
		Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
	}}},
	// The registry was cancelled. Its free text reason goes in the email only.
	"registry.cancelled": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/registry_cancelled", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Your Hadia registry was cancelled" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"),
				"gift_count": mStr(p, "gift_count"), "reason": mStr(p, "reason")}
		},
		WATemplate: "hadia_registry_cancelled_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "registry") },
	}}},
	"registry.closed": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/registry_closed", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Your Hadia registry is closed" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"), "expired": mStr(p, "reason") == "expired"}
		},
		WATemplate: "hadia_registry_closed_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "registry") },
	}}},
	// Guests give no email, so a refund notice needs their phone. No link: the guest has nothing to do.
	"refund.pending": {{maskaniMapping: maskaniMapping{
		TemplateID: "hadia/refund_pending", PhoneKey: "guest_phone",
		Subject: func(map[string]any) string { return "Your Hadia gift is being refunded" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["guest_name"], "there"), "registry": mStr(p, "registry_title"),
				"amount": mMoney(p, "amount"), "fee_kept": mMoneyOr(p, "fee_kept", "KES 0")}
		},
		WATemplate: "hadia_refund_pending_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "registry", "amount", "fee_kept") },
		Skip:       func(p map[string]any) bool { return mStr(p, "guest_phone") == "" },
	}}},
	"refund.completed": {{maskaniMapping: maskaniMapping{
		TemplateID: "hadia/refund_completed", PhoneKey: "guest_phone",
		Subject: func(map[string]any) string { return "Your Hadia gift was refunded" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["guest_name"], "there"), "registry": mStr(p, "registry_title"),
				"amount": mMoney(p, "amount"), "reference": waParam(p["reference"], "-")}
		},
		WATemplate: "hadia_refund_completed_v2",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "amount", "registry", "reference") },
		Skip:       func(p map[string]any) bool { return mStr(p, "guest_phone") == "" },
	}}},
	"premium.activated": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/premium_activated", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Premium is on for your Hadia registry" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"), "amount": mMoney(p, "amount")}
		},
		WATemplate: "hadia_premium_activated_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "registry", "amount") },
	}}},
	// A new case in the exception queue, for Hadia's staff. hadia-api names who should act
	// (responders) but owns the staff list, so it goes to the tenant's contact details. The summary
	// stays in the email; high and urgent cases also go by WhatsApp, without it.
	// hadia.ops.health_daily and hadia.ops.health_weekly are not mapped here: they are for platform
	// ops, not Hadia's staff, and internal/modules/events (hadia_ops_health.go) sends them on the ops
	// channels. This consumer acks them as unmapped.
	"case.opened": {{
		FixedPath: "admin/exceptions",
		WhatsAppIf: func(p map[string]any) bool {
			priority := mStr(p, "priority")
			return priority == "urgent" || priority == "high"
		},
		maskaniMapping: maskaniMapping{
			TemplateID: "hadia/case_opened", Staff: true,
			Subject: func(p map[string]any) string {
				return "New " + mStr(p, "priority") + " case in the Hadia queue: " + hadiaCaseLabel(mStr(p, "kind"))
			},
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				return map[string]any{"name": "Hadia team", "priority": mStr(p, "priority"), "kind": hadiaCaseLabel(mStr(p, "kind")),
					"summary": mStr(p, "summary"), "case_id": mStr(p, "case_id")}
			},
			WATemplate: "hadia_case_opened_v1_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "priority", "kind") },
		},
	}},
	// The commission sweep, for Hadia's finance contact. A changed destination could be a stolen login,
	// so it alone also goes by WhatsApp.
	"finance.alert": {{
		FixedPath:  "admin/exceptions",
		WhatsAppIf: func(p map[string]any) bool { return mStr(p, "kind") == "sweep_destination_changed" },
		maskaniMapping: maskaniMapping{
			TemplateID: "hadia/finance_alert", Staff: true,
			Subject: func(p map[string]any) string {
				if mStr(p, "kind") == "sweep_destination_changed" {
					return "The Hadia commission sweep has a new destination"
				}
				return "The Hadia commission sweep started"
			},
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				return map[string]any{"name": "Hadia finance", "summary": mStr(p, "summary"), "reference": mStr(p, "reference")}
			},
			WATemplate: "hadia_finance_alert_v1_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name") },
		},
	}},
	// The daily count of open cases. WhatsApp only when something urgent is waiting.
	"case.summary": {{
		FixedPath:  "admin/exceptions",
		WhatsAppIf: func(p map[string]any) bool { return mPositive(p, "urgent") },
		maskaniMapping: maskaniMapping{
			TemplateID: "hadia/case_summary", Staff: true,
			Subject: func(p map[string]any) string { return "Hadia exception queue for " + mStr(p, "day") },
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				return map[string]any{"name": "Hadia team", "day": mStr(p, "day"), "open": fmt.Sprint(p["open"]), "in_progress": fmt.Sprint(p["in_progress"]),
					"overdue": fmt.Sprint(p["overdue"]), "urgent": fmt.Sprint(p["urgent"])}
			},
			WATemplate: "hadia_case_summary_v1_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "urgent", "open", "overdue") },
		},
	}},
	// Staff changed a registry. The reason is free text, so it stays in the email.
	"registry.moderated": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/registry_moderated", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Hadia changed your registry" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"),
				"action": mStr(p, "action"), "reason": mStr(p, "reason")}
		},
		WATemplate: "hadia_registry_moderated_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "registry", "action") },
	}}},
	// A data request has a legal deadline, so staff hear at once. The payload carries no contact details.
	"privacy.requested": {{
		FixedPath: "admin/privacy",
		maskaniMapping: maskaniMapping{
			TemplateID: "hadia/privacy_requested", Staff: true,
			Subject: func(map[string]any) string { return "A new Hadia data request" },
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				return map[string]any{"name": "Hadia team", "kind": mStr(p, "kind"), "due_at": mTime(p, "due_at")}
			},
			WATemplate: "hadia_privacy_requested_v1_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "kind", "due_at") },
		},
	}},
	"privacy.completed": {{PathKey: "status_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/privacy_completed", PhoneKey: "phone", EmailKey: "email",
		Subject: func(map[string]any) string { return "Your Hadia data request" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": "there", "kind": mStr(p, "kind"), "status": mStr(p, "status")}
		},
		WATemplate: "hadia_privacy_completed_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "kind", "status") },
	}}},
	// The owner released the reservation or the hold ended. No link: there is nothing to manage.
	"reservation.released": {
		{maskaniMapping: maskaniMapping{
			TemplateID: "hadia/reservation_released", PhoneKey: "guest_phone", EmailKey: "guest_email",
			Subject:    func(map[string]any) string { return "Your gift reservation has ended" },
			Data:       hadiaGuestData,
			WATemplate: "hadia_reservation_released_v1",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "item") },
		}},
		// A guest let the gift go (owner, 9 November 2026): the owner hears it, with the guest's
		// optional note. Email only: no approved WhatsApp template carries a free note.
		{PathKey: "owner_url", maskaniMapping: maskaniMapping{
			TemplateID: "hadia/reservation_released_owner", EmailKey: "owner_email",
			Subject: func(map[string]any) string { return "A reserved gift is available again" },
			Data: func(p map[string]any, _ *tenantInfo) map[string]any {
				guest := "A guest"
				if b, _ := p["show_reserver_names"].(bool); b {
					guest = waParam(p["guest_name"], "A guest")
				}
				return map[string]any{"name": waParam(p["owner_name"], "there"), "guest": guest,
					"item": mStr(p, "item_title"), "registry": mStr(p, "registry_title"), "note": mStr(p, "note")}
			},
			Skip: func(p map[string]any) bool { return mStr(p, "reason") != "cancelled" || mStr(p, "owner_email") == "" },
		}},
	},
}

// startHadiaConsumer subscribes to hadia.> on hadia-api's stream "hadia". digest holds the alerts of
// owners who chose a daily summary; nil (no database) sends every alert at once.
func startHadiaConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, digest *hadiaDigest, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping hadia consumer: NATS not available")
		return
	}
	log := logg.Named("hadia")
	handler := func(m *nats.Msg) {
		var evt eventslib.Event
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			log.Error("hadia event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		msgs, ok := hadiaMappings[evt.EventType]
		if !ok || evt.TenantID == uuid.Nil {
			_ = m.Ack()
			return
		}
		tenantID := evt.TenantID.String()
		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			log.Error("hadia event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		for i, msg := range msgs {
			if msg.Skip != nil && msg.Skip(evt.Payload) {
				continue
			}
			if heldForDigest(msg, evt.Payload, digest) {
				// Held once per event id, so a redelivery holds nothing new.
				if err := digest.hold(ctx, ti, evt); err != nil {
					log.Error("hadia event: gift not held for the daily summary", zap.String("type", evt.EventType), zap.Error(err))
					_ = m.Nak()
					return
				}
				continue
			}
			// Idempotency keys carry the message index, so a redelivered event never sends twice
			// and the guest and owner messages of one event never collide.
			if err := dispatchHadia(ctx, nc, cfg, ti, tenantID, evt, msg, i); err != nil {
				log.Error("hadia event: dispatch failed", zap.String("type", evt.EventType), zap.Error(err))
				_ = m.Nak()
				return
			}
		}
		_ = m.Ack()
	}
	eventslib.SubscribeQueueWithRebind(logg, js, "hadia", "hadia.>", "notifications-hadia", handler,
		nats.BindStream("hadia"),
		nats.Durable("notifications-hadia"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
	// One-time codes go out on hadia-api's short-lived stream hadia_secret (15 minute retention), so
	// a code never sits in the 7-day hadia stream. Same handler: the event type is otp.requested.
	eventslib.SubscribeQueueWithRebind(logg, js, "hadia_secret", "hadia_secret.otp.requested", "notifications-hadia-secret", handler,
		nats.BindStream("hadia_secret"),
		nats.Durable("notifications-hadia-secret"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}

// dispatchHadia queues one message by email and WhatsApp through sendToParties.
func dispatchHadia(ctx context.Context, nc *nats.Conn, cfg *config.Config, ti *tenantInfo, tenantID string, evt eventslib.Event, msg hadiaMessage, index int) error {
	p := evt.Payload
	data := msg.Data(p, ti)
	data["brand_note"] = hadiaTrustNote
	suffix := msg.FixedPath
	if msg.PathKey != "" {
		suffix = hadiaPath(mStr(p, msg.PathKey))
	}
	if suffix != "" {
		data["action_link"] = hadiaEmailBase(ti) + "/" + suffix
	}
	scope := messaging.SenderScopeTenant
	if msg.Platform {
		scope = messaging.SenderScopePlatform
	}
	target, email, phone := messaging.TargetCustomer, mStr(p, msg.EmailKey), mStr(p, msg.PhoneKey)
	if msg.Staff {
		target, email, phone = messaging.TargetStaff, ti.ContactEmail, ti.ContactPhone
	}
	if email == "" && phone == "" {
		return nil // nobody to tell; redelivering would not change that
	}
	party := maskaniParty(msg.maskaniMapping, target, "", email, phone, nil, data, suffix)
	// A _btn template needs its button; without a hadiafrica.com link the message goes by email only.
	if party.WhatsApp != nil && strings.HasSuffix(party.WhatsApp.Name, "_btn") && suffix == "" {
		party.WhatsApp = nil
	}
	if msg.WhatsAppIf != nil && !msg.WhatsAppIf(p) {
		party.WhatsApp = nil
	}
	return sendToParties(ctx, nc, cfg, partyNotice{
		ServiceID:    "hadia",
		TenantID:     tenantID,
		TemplateID:   msg.TemplateID,
		WATemplateID: msg.WATemplateID,
		Subject:      msg.Subject(p),
		Scope:        scope,
		Data:         data,
		Country:      ti.Country,
		IdemBase:     fmt.Sprintf("hadia-%s-%d", evt.ID, index),
		Parties:      []noticeParty{party},
	})
}
