package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/contacts"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/moneyfmt"
)

// posEvent is the shared-events envelope from pos-api.
type posEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

// posNotice is how one pos-api event reaches its customer: an email template and a WhatsApp
// template (UTILITY, no link, so nothing to button), sent through sendToParties on whichever of
// the two the customer can be reached on. SMS is not an active channel and is never used.
//
// Sale receipts are not here: pos.sale.notification_requested is handled by
// internal/modules/events (handleSaleNotification), which picks email or WhatsApp per request.
type posNotice struct {
	TemplateID string // templates/email/<id>.html and templates/whatsapp/<id>.txt
	Subject    string
	WAName     string // approved WhatsApp template (templatesync/templates.json)
	EmailKey   string // payload field holding the customer's email
	PhoneKey   string // payload field holding the customer's phone
	NameKey    string // payload field holding the customer's name
	// Build returns the template data and the WhatsApp body parameters, in {{1}}..{{n}} order.
	Build func(p map[string]any, business string) (data map[string]any, waParams []string)
}

var posNotices = map[string]posNotice{
	// View Payments > "Send payment received notification" for one payment.
	"payment.received_notification": {
		TemplateID: "pos/pos_payment_receipt",
		Subject:    "Payment received",
		WAName:     "pos_payment_received_v1",
		EmailKey:   "customer_email",
		PhoneKey:   "customer_phone",
		NameKey:    "customer_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			currency := payStr(p, "currency")
			amount := moneyfmt.Format(payNum(p, "amount"), currency)
			balance := payNum(p, "total_amount") - payNum(p, "paid_total")
			balanceText := "fully paid"
			if balance > 0.009 {
				balanceText = moneyfmt.Format(balance, currency)
			}
			data := map[string]any{
				"order_number": payStr(p, "order_number"),
				"amount":       amount,
				"method":       humanize(payStr(p, "method")),
				"reference":    payStr(p, "reference"),
				"paid_at":      payDate(p, "paid_at"),
				"balance":      balanceText,
			}
			return data, []string{nonEmptyOr(payStr(p, "customer_name"), "there"), amount, nonEmptyOr(payStr(p, "order_number"), "your order"), business, balanceText}
		},
	},
	"loyalty.points.earned": {
		TemplateID: "pos/loyalty_points_earned",
		Subject:    "You earned loyalty points",
		WAName:     "pos_loyalty_points_v1",
		EmailKey:   "customer_email",
		PhoneKey:   "customer_phone",
		NameKey:    "customer_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			points, balance := payInt(p, "points_earned"), payInt(p, "balance_after")
			return map[string]any{"points_earned": points, "balance_after": balance},
				[]string{nonEmptyOr(payStr(p, "customer_name"), "there"), strconv.FormatInt(points, 10), business, strconv.FormatInt(balance, 10)}
		},
	},
	"loyalty.tier_upgraded": {
		TemplateID: "pos/loyalty_tier_upgraded",
		Subject:    "Your loyalty tier has gone up",
		WAName:     "pos_loyalty_tier_v1",
		EmailKey:   "customer_email",
		PhoneKey:   "customer_phone",
		NameKey:    "customer_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			tier := humanize(payStr(p, "new_tier"))
			return map[string]any{"new_tier": tier, "lifetime_points": payInt(p, "lifetime_points")},
				[]string{nonEmptyOr(payStr(p, "customer_name"), "there"), business, nonEmptyOr(tier, "a new tier")}
		},
	},
	"loyalty.referral_earned": {
		TemplateID: "pos/loyalty_referral_earned",
		Subject:    "You earned a referral bonus",
		WAName:     "pos_loyalty_referral_v1",
		EmailKey:   "referrer_email",
		PhoneKey:   "referrer_phone",
		NameKey:    "referrer_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			bonus, balance := payInt(p, "bonus_points"), payInt(p, "balance_after")
			return map[string]any{"bonus_points": bonus, "balance_after": balance},
				[]string{nonEmptyOr(payStr(p, "referrer_name"), "there"), business, strconv.FormatInt(bonus, 10), strconv.FormatInt(balance, 10)}
		},
	},
	"return.completed": {
		TemplateID: "pos/return_completed",
		Subject:    "Your refund has been processed",
		WAName:     "pos_refund_processed_v1",
		EmailKey:   "customer_email",
		PhoneKey:   "customer_phone",
		NameKey:    "customer_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			amount := moneyfmt.Format(payNum(p, "refund_amount"), payStr(p, "currency"))
			ret := nonEmptyOr(payStr(p, "return_number"), payStr(p, "order_number"))
			return map[string]any{
					"refund_amount": amount, "return_number": payStr(p, "return_number"),
					"order_number": payStr(p, "order_number"), "refund_ref": payStr(p, "treasury_refund_ref"),
				},
				[]string{nonEmptyOr(payStr(p, "customer_name"), "there"), amount, nonEmptyOr(ret, "your return"), business}
		},
	},
	// Daily reminder the day before a layaway instalment is due (pos-api layaway_reminders).
	"layaway.payment_due": {
		TemplateID: "pos/layaway_payment_due",
		Subject:    "Your layaway payment is due tomorrow",
		WAName:     "pos_layaway_due_v1",
		EmailKey:   "customer_email",
		PhoneKey:   "customer_phone",
		NameKey:    "customer_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			balance := moneyfmt.Format(payNum(p, "balance_due"), payStr(p, "currency"))
			due := payDate(p, "due_date")
			return map[string]any{"balance_due": balance, "due_date": due},
				[]string{nonEmptyOr(payStr(p, "customer_name"), "there"), business, nonEmptyOr(due, "tomorrow"), balance}
		},
	},
	"hotel.check_in": {
		TemplateID: "pos/hotel_check_in",
		Subject:    "Your check-in is confirmed",
		WAName:     "pos_hotel_check_in_v1",
		EmailKey:   "guest_email",
		PhoneKey:   "guest_phone",
		NameKey:    "guest_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			room := payStr(p, "room_number")
			nights := payInt(p, "nights")
			charge := moneyfmt.Format(payNum(p, "total_charge"), payStr(p, "currency"))
			return map[string]any{"room_number": room, "nights": nights, "total_charge": charge, "check_in": payDate(p, "check_in_date")},
				[]string{nonEmptyOr(payStr(p, "guest_name"), "there"), business, nonEmptyOr(room, "your room"), strconv.FormatInt(max(nights, 1), 10)}
		},
	},
	"hotel.check_out": {
		TemplateID: "pos/hotel_check_out",
		Subject:    "Your stay summary",
		WAName:     "pos_hotel_check_out_v1",
		EmailKey:   "guest_email",
		PhoneKey:   "guest_phone",
		NameKey:    "guest_name",
		Build: func(p map[string]any, business string) (map[string]any, []string) {
			total := moneyfmt.Format(payNum(p, "total_folio"), payStr(p, "currency"))
			return map[string]any{"total_folio": total, "checked_out": payDate(p, "checked_out_at")},
				[]string{nonEmptyOr(payStr(p, "guest_name"), "there"), business, total}
		},
	},
}

// withCRMRecipient puts the CRM contact's name, email and phone into the notice's payload fields
// when the event carries crm_contact_id.
//
// Contract with pos-api: crm_contact_id is always the contact of the person the notice goes to.
// For loyalty.referral_earned that is the REFERRER (the notice reads the referrer_* fields), never
// the referred friend. pos-api sends name, email and phone only for people with no CRM contact;
// for a linked person the event carries the id alone and every detail comes from the CRM. When
// both are present the CRM wins field by field.
//
// For hotel.check_in and hotel.check_out the CRM details replace what the guest gave at the desk
// for the notice only; the guest register in pos-api keeps what the guest wrote.
//
// The error is set when the CRM could not answer. The event passed in is not changed; a copy is
// returned.
func withCRMRecipient(ctx context.Context, res *contacts.Resolver, evt posEvent, ti *tenantInfo) (posEvent, error) {
	n, ok := posNotices[evt.EventType]
	if !ok || res == nil {
		return evt, nil
	}
	contactID := contacts.ParseID(payStr(evt.Payload, "crm_contact_id"))
	if contactID == uuid.Nil {
		return evt, nil
	}
	tenantID := contacts.ParseID(evt.TenantID)
	if ti != nil && ti.ID != uuid.Nil {
		tenantID = ti.ID
	}
	rec, err := res.Lookup(ctx, tenantID, contactID, contacts.Recipient{
		Name:  payStr(evt.Payload, n.NameKey),
		Email: payStr(evt.Payload, n.EmailKey),
		Phone: payStr(evt.Payload, n.PhoneKey),
	})
	p := make(map[string]any, len(evt.Payload)+3)
	for k, v := range evt.Payload {
		p[k] = v
	}
	p[n.NameKey], p[n.EmailKey], p[n.PhoneKey] = rec.Name, rec.Email, rec.Phone
	evt.Payload = p
	return evt, err
}

// posNoticeFor resolves who the event's notice goes to and builds it. retry is true when the
// event names a CRM contact, there is no email or phone to send to, and the CRM could not be
// reached: the message is redelivered later instead of being dropped.
func posNoticeFor(ctx context.Context, res *contacts.Resolver, evt posEvent, ti *tenantInfo) (notice partyNotice, ok, retry bool) {
	resolved, lookupErr := withCRMRecipient(ctx, res, evt, ti)
	notice, ok = posPartyNotice(resolved, ti)
	return notice, ok, !ok && lookupErr != nil
}

// posPartyNotice builds the notice for one event, or false when there is no template for the
// event or no customer contact on it.
func posPartyNotice(evt posEvent, ti *tenantInfo) (partyNotice, bool) {
	n, ok := posNotices[evt.EventType]
	if !ok {
		return partyNotice{}, false
	}
	email, phone := strings.TrimSpace(payStr(evt.Payload, n.EmailKey)), strings.TrimSpace(payStr(evt.Payload, n.PhoneKey))
	if email == "" && phone == "" {
		return partyNotice{}, false
	}
	business := "us"
	if ti != nil && ti.Name != "" {
		business = ti.Name
	}
	data, params := n.Build(evt.Payload, business)
	data["name"] = nonEmptyOr(payStr(evt.Payload, n.NameKey), "Customer")
	data["business_name"] = business
	base := evt.ID
	if base == "" {
		base = evt.EventType + "-" + evt.AggregateID
	}
	country := ""
	if ti != nil {
		country = ti.Country
	}
	return partyNotice{
		ServiceID:  "pos",
		TenantID:   evt.TenantID,
		TemplateID: n.TemplateID,
		Subject:    n.Subject,
		Data:       data,
		Country:    country,
		IdemBase:   "pos-" + base,
		Parties: []noticeParty{{
			Target:   messaging.TargetCustomer,
			Email:    email,
			Phone:    phone,
			WhatsApp: &partyWhatsApp{Name: n.WAName, Params: params},
		}},
	}, true
}

// startPosConsumer subscribes to pos.> events and sends the customer notices in posNotices.
func startPosConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, crmContacts *contacts.Resolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping pos consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt posEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("pos event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		if _, ok := posNotices[evt.EventType]; !ok || evt.TenantID == "" {
			_ = m.Ack()
			return
		}
		ti, err := tr.resolve(ctx, evt.TenantID)
		if err != nil {
			logg.Error("pos event: failed to resolve tenant", zap.String("tenant_id", evt.TenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		notice, ok, retry := posNoticeFor(ctx, crmContacts, evt, ti)
		contactID := payStr(evt.Payload, "crm_contact_id")
		if retry {
			retryCRMLookup(m, logg, "pos", evt.EventType, evt.TenantID, contactID)
			return
		}
		if !ok {
			if contacts.ParseID(contactID) != uuid.Nil {
				logg.Info("pos event: linked contact has no email or phone in the CRM, notice dropped",
					zap.String("type", evt.EventType), zap.String("tenant_id", evt.TenantID),
					zap.String("crm_contact_id", contactID))
			} else {
				logg.Debug("pos event: no customer contact, nothing to send", zap.String("type", evt.EventType))
			}
			_ = m.Ack()
			return
		}
		if err := sendToParties(ctx, nc, cfg, notice); err != nil {
			logg.Error("pos event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}
		logg.Info("pos notification dispatched", zap.String("type", evt.EventType), zap.String("template", notice.TemplateID))
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "pos", "pos.>", "notifications-pos-orders", handler,
		nats.BindStream("pos"),
		nats.Durable("notifications-pos-orders"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(crmRetryMaxDeliver),
	)
}

// payStr reads a string field from an event payload ("" when absent or not a string).
func payStr(p map[string]any, key string) string {
	switch v := p[key].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// payNum reads a numeric field that may arrive as a JSON number or a decimal string.
func payNum(p map[string]any, key string) float64 {
	switch v := p[key].(type) {
	case float64:
		return v
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f
	}
	return 0
}

func payInt(p map[string]any, key string) int64 { return int64(payNum(p, key)) }

// payDate shows an RFC 3339 or yyyy-mm-dd payload date as "2 Nov 2026", or the raw value.
func payDate(p map[string]any, key string) string {
	raw := strings.TrimSpace(payStr(p, key))
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Format("2 Jan 2006")
		}
	}
	return raw
}

// humanize turns a code such as "mpesa" or "gold_plus" into "Mpesa" / "Gold plus".
func humanize(code string) string {
	code = strings.TrimSpace(strings.ReplaceAll(code, "_", " "))
	if code == "" {
		return ""
	}
	return strings.ToUpper(code[:1]) + code[1:]
}

// nonEmptyOr returns v trimmed, or fallback when blank: Meta refuses a template send with an empty
// body parameter.
func nonEmptyOr(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}
