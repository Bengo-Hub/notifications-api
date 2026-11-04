package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// Hadia (gift registry, one client tenant) notifications. hadia-api publishes hadia.<event_type>
// through its outbox (hadia-api docs/architecture.md, "Published by hadia-api"). One event can reach
// more than one person, such as a reservation that goes to the guest and to the owner, so each event
// maps to a list of messages. Every link is a WhatsApp URL button on hadiafrica.com, never body text
// (docs/whatsapp-template-policy.md). Hadia is a single tenant on its own domain, so button paths
// carry no tenant slug.

// hadiaButtonBase is the domain fixed in every hadia_*_btn template.
const hadiaButtonBase = "https://hadiafrica.com"

// hadiaMessage is one message an event sends. It reuses maskaniMapping's fields (template,
// recipient keys, data, WhatsApp template and parameters); PathKey names the payload field holding
// the full hadiafrica.com link the button and email open.
type hadiaMessage struct {
	maskaniMapping
	PathKey string
}

// hadiaPath turns a full hadiafrica.com link into the button suffix ("r/AB12CD34/reserved/tok").
// A link on any other host gives no button, so a payload can never point a button elsewhere.
func hadiaPath(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Host == "" {
		return ""
	}
	base, _ := url.Parse(hadiaButtonBase)
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	if host != base.Host {
		return ""
	}
	return strings.Trim(u.Path, "/")
}

func hadiaGuestData(p map[string]any, _ *tenantInfo) map[string]any {
	return map[string]any{"name": waParam(p["guest_name"], "there"), "item": mStr(p, "item_title"),
		"registry": mStr(p, "registry_title"), "expires_at": mTime(p, "expires_at"), "owner": waParam(p["owner_name"], "the couple")}
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
			WATemplate: "hadia_reservation_owner_alert_v1_btn",
			WAParams:   func(d map[string]any) []string { return mParams(d, "name", "guest", "item") },
			Skip:       func(p map[string]any) bool { return mStr(p, "owner_phone") == "" && mStr(p, "owner_email") == "" },
		}},
	},
	// Two days before a hold ends.
	"reservation.reminder": {{PathKey: "manage_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/reservation_reminder", PhoneKey: "guest_phone", EmailKey: "guest_email",
		Subject:    func(map[string]any) string { return "Your gift reservation ends soon" },
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
	// phone is known (guests give no email), and an alert to the owner.
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
		{PathKey: "owner_url", maskaniMapping: maskaniMapping{
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
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"),
				"gross": mMoney(p, "gross"), "commission": mMoneyOr(p, "commission", "KES 0"), "net": mMoney(p, "net"),
				"reference": waParam(p["reference"], "-")}
		},
		WATemplate: "hadia_payout_completed_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "net", "registry", "reference") },
	}}},
	// A payout failed and the money went back to the registry (from treasury.escrow.dispatch_failed).
	// The provider's reason stays out of the message: it is technical and Hadia staff act on it.
	"payout.delayed": {{PathKey: "owner_url", maskaniMapping: maskaniMapping{
		TemplateID: "hadia/payout_delayed", PhoneKey: "owner_phone", EmailKey: "owner_email",
		Subject: func(map[string]any) string { return "Your Hadia payout is delayed" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["owner_name"], "there"), "registry": mStr(p, "registry_title"), "net": mMoney(p, "net")}
		},
		WATemplate: "hadia_payout_delayed_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "net", "registry") },
	}}},
	// The owner released the reservation or the hold ended. No link: there is nothing to manage.
	"reservation.released": {{maskaniMapping: maskaniMapping{
		TemplateID: "hadia/reservation_released", PhoneKey: "guest_phone", EmailKey: "guest_email",
		Subject:    func(map[string]any) string { return "Your gift reservation has ended" },
		Data:       hadiaGuestData,
		WATemplate: "hadia_reservation_released_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "item") },
	}}},
}

// startHadiaConsumer subscribes to hadia.> on hadia-api's stream "hadia".
func startHadiaConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
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
	suffix := ""
	if msg.PathKey != "" {
		if path := hadiaPath(mStr(p, msg.PathKey)); path != "" {
			suffix = path
			data["action_link"] = hadiaButtonBase + "/" + path
		}
	}
	scope := messaging.SenderScopeTenant
	if msg.Platform {
		scope = messaging.SenderScopePlatform
	}
	party := maskaniParty(msg.maskaniMapping, messaging.TargetCustomer, "", mStr(p, msg.EmailKey), mStr(p, msg.PhoneKey), nil, data, suffix)
	// A _btn template needs its button; without a hadiafrica.com link the message goes by email only.
	if party.WhatsApp != nil && strings.HasSuffix(party.WhatsApp.Name, "_btn") && suffix == "" {
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
