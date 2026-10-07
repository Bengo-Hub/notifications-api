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
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/moneyfmt"
)

// Maskani (property platform) notifications. maskani-api publishes maskani.<event_type> through its
// outbox. Each mapped event goes out by email (when the payload has an address) and by WhatsApp
// (approved UTILITY template; any link is the template's URL button on maskaniapp, never body text).
// Customer messages go to the owner, occupant, buyer or visitor in the payload; staff alerts go to
// the tenant contact email and phone. Notices to an audience are sent by maskani-api through the
// send API, not from events.

// maskaniButtonBase is the domain fixed in every maskani_*_btn template (docs/whatsapp-template-policy.md).
const maskaniButtonBase = "https://maskaniapp.codevertexafrica.com"

// maskaniTZ renders times for East African estates (UTC+3, no daylight saving).
var maskaniTZ = time.FixedZone("EAT", 3*3600)

type maskaniMapping struct {
	TemplateID string // templates/{email,whatsapp}/<TemplateID>
	Subject    func(p map[string]any) string
	Staff      bool   // true: tenant contacts; false: the payload's own recipient
	PhoneKey   string // payload key of the customer's phone
	EmailKey   string // payload key of the customer's email
	// Path is the page under /{slug}/ the button and email link open ("" for none).
	Path func(p map[string]any) string
	// Data builds the template data shared by both channels.
	Data func(p map[string]any, ti *tenantInfo) map[string]any
	// WhatsApp template name and its body parameters, in {{1}}..{{n}} order.
	WATemplate string
	WAParams   func(d map[string]any) []string
	Skip       func(p map[string]any) bool
}

func mStr(p map[string]any, key string) string {
	switch v := p[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// mMoney formats a decimal string amount ("6450.00") as "KES 6,450".
func mMoney(p map[string]any, key string) string {
	f, err := strconv.ParseFloat(mStr(p, key), 64)
	if err != nil {
		return mStr(p, key)
	}
	return moneyfmt.Format(f, "KES")
}

// mTime renders an RFC3339 time as "11 Oct 14:32" in East Africa Time.
func mTime(p map[string]any, key string) string {
	s := mStr(p, key)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.In(maskaniTZ).Format("2 Jan 15:04")
	}
	return s
}

func mParams(d map[string]any, keys ...string) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = waParam(d[k], "-")
	}
	return out
}

func fixed(path string) func(map[string]any) string {
	return func(map[string]any) string { return path }
}

// withID links to prefix/<payload[key]>, or nowhere when the id is missing (never a broken link).
func withID(prefix, key string) func(map[string]any) string {
	return func(p map[string]any) string {
		if id := mStr(p, key); id != "" {
			return prefix + "/" + id
		}
		return ""
	}
}

var maskaniMappings = map[string]maskaniMapping{
	"bill.issued": {
		TemplateID: "maskani/bill_issued", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal"),
		Subject: func(p map[string]any) string {
			return fmt.Sprintf("Your %s bill for %s", mStr(p, "period"), mStr(p, "account_ref"))
		},
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there"), "period": mStr(p, "period"),
				"account_ref": mStr(p, "account_ref"), "unit_code": mStr(p, "unit_code"), "amount": mMoney(p, "amount"),
				"due_date": mStr(p, "due_date"), "paybill": mStr(p, "paybill"), "invoice_number": mStr(p, "invoice_number")}
		},
		WATemplate: "maskani_bill_issued_v1_btn",
		WAParams: func(d map[string]any) []string {
			return mParams(d, "name", "period", "account_ref", "amount", "due_date", "paybill", "account_ref")
		},
	},
	"payment.applied": {
		TemplateID: "maskani/payment_received", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal"),
		Subject: func(p map[string]any) string { return "Payment received for " + mStr(p, "account_ref") },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there"), "amount": mMoney(p, "amount"),
				"account_ref": mStr(p, "account_ref"), "receipt": mStr(p, "receipt"), "balance": mMoney(p, "balance"),
				"method": mStr(p, "method")}
		},
		WATemplate: "maskani_payment_received_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "amount", "account_ref", "receipt", "balance") },
	},
	"pass.created": {
		TemplateID: "maskani/visitor_pass", PhoneKey: "visitor_phone", EmailKey: "visitor_email",
		Subject: func(p map[string]any) string { return "Your gate code" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"visitor_name": waParam(p["visitor_name"], "there"), "estate": ti.Name,
				"code": mStr(p, "code"), "valid_from": mTime(p, "valid_from"), "valid_to": mTime(p, "valid_to")}
		},
		WATemplate: "maskani_visitor_pass_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "visitor_name", "estate", "code", "valid_from", "valid_to") },
		// A pass without a code (QR only) has nothing to send.
		Skip: func(p map[string]any) bool { return mStr(p, "code") == "" },
	},
	"visitor.arrived": {
		TemplateID: "maskani/visitor_arrived", PhoneKey: "host_phone", EmailKey: "host_email",
		Subject: func(p map[string]any) string { return mStr(p, "visitor_name") + " has arrived at the gate" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"host_name": waParam(p["host_name"], "there"), "visitor_name": waParam(p["visitor_name"], "Your visitor"),
				"unit_code": mStr(p, "unit_code"), "time": mTime(p, "occurred_at"), "vehicle_plate": mStr(p, "vehicle_plate")}
		},
		WATemplate: "maskani_visitor_arrived_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "host_name", "visitor_name", "unit_code", "time") },
	},
	"walk_in.requested": {
		TemplateID: "maskani/walk_in_request", PhoneKey: "host_phone", EmailKey: "host_email",
		Path:    withID("portal/walk-ins", "event_id"),
		Subject: func(p map[string]any) string { return "Visitor at the gate for " + mStr(p, "unit_code") },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"host_name": waParam(p["host_name"], "there"), "visitor_name": waParam(p["visitor_name"], "A visitor"),
				"unit_code": mStr(p, "unit_code")}
		},
		WATemplate: "maskani_walk_in_request_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "host_name", "visitor_name", "unit_code") },
	},
	"party.invited": {
		TemplateID: "maskani/portal_invite", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal"),
		Subject: func(p map[string]any) string { return "Your owner portal is ready" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there"), "estate": ti.Name}
		},
		WATemplate: "maskani_portal_invite_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "estate") },
	},
	"sale_contract.activated": {
		TemplateID: "maskani/contract_activated", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal/purchase"),
		Subject: func(p map[string]any) string { return "Sale agreement " + mStr(p, "contract_number") + " is active" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["buyer"], "there"), "contract_number": mStr(p, "contract_number"),
				"unit_code": mStr(p, "unit_code"), "account_ref": mStr(p, "account_ref"), "net_price": mMoney(p, "net_price")}
		},
		WATemplate: "maskani_contract_activated_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "contract_number", "unit_code", "account_ref") },
	},
	"sale_contract.fully_paid": {
		TemplateID: "maskani/contract_fully_paid", PhoneKey: "phone", EmailKey: "email", Path: fixed("portal/purchase"),
		Subject: func(p map[string]any) string { return "Sale agreement " + mStr(p, "contract_number") + " is fully paid" },
		Data: func(p map[string]any, _ *tenantInfo) map[string]any {
			return map[string]any{"name": waParam(p["name"], "there"), "contract_number": mStr(p, "contract_number"),
				"account_ref": mStr(p, "account_ref")}
		},
		WATemplate: "maskani_contract_fully_paid_v1",
		WAParams:   func(d map[string]any) []string { return mParams(d, "name", "contract_number", "account_ref") },
	},
	"incident.reported": {
		TemplateID: "maskani/incident_alert", Staff: true,
		Path:    withID("security/incidents", "incident_id"),
		Subject: func(p map[string]any) string { return "Security alert: " + mStr(p, "title") },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "severity": mStr(p, "severity"), "title": mStr(p, "title"),
				"number": mStr(p, "number"), "category": mStr(p, "category")}
		},
		WATemplate: "maskani_incident_alert_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "severity", "title", "number") },
		Skip:       func(p map[string]any) bool { urgent, _ := p["urgent"].(bool); return !urgent },
	},
	"work_order.sla_breached": {
		TemplateID: "maskani/work_order_sla_breached", Staff: true,
		Path:    withID("works", "work_order_id"),
		Subject: func(p map[string]any) string { return "Work order " + mStr(p, "number") + " is past its SLA" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			return map[string]any{"estate": ti.Name, "number": mStr(p, "number"), "priority": mStr(p, "priority"),
				"title": mStr(p, "title"), "due_at": mTime(p, "resolution_due_at")}
		},
		WATemplate: "maskani_work_order_overdue_v1_btn",
		WAParams:   func(d map[string]any) []string { return mParams(d, "estate", "number", "priority", "title") },
	},
	// Email only: a renewal reminder is not urgent enough for WhatsApp.
	"vendor.document_expiring": {
		TemplateID: "maskani/vendor_document_expiring", Staff: true,
		Path:    withID("vendors", "vendor_id"),
		Subject: func(p map[string]any) string { return mStr(p, "vendor") + ": " + mStr(p, "doc_type") + " expires soon" },
		Data: func(p map[string]any, ti *tenantInfo) map[string]any {
			expires := mStr(p, "expires_at")
			if t, err := time.Parse(time.RFC3339Nano, expires); err == nil {
				expires = t.In(maskaniTZ).Format("2 Jan 2006")
			}
			return map[string]any{"estate": ti.Name, "vendor": mStr(p, "vendor"), "doc_type": mStr(p, "doc_type"),
				"days_left": mStr(p, "days_left"), "expires_at": expires}
		},
	},
}

// startMaskaniConsumer subscribes to maskani.> and sends each mapped event by email and WhatsApp.
func startMaskaniConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping maskani consumer: NATS not available")
		return
	}
	log := logg.Named("maskani")

	handler := func(m *nats.Msg) {
		var evt eventslib.Event
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			log.Error("maskani event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}
		mp, ok := maskaniMappings[evt.EventType]
		if !ok || (mp.Skip != nil && mp.Skip(evt.Payload)) {
			_ = m.Ack()
			return
		}
		if evt.TenantID == uuid.Nil {
			_ = m.Ack()
			return
		}
		tenantID := evt.TenantID.String()
		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			log.Error("maskani event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}
		if err := dispatchMaskani(ctx, nc, cfg, ti, tenantID, evt, mp); err != nil {
			log.Error("maskani event: dispatch failed", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "maskani", "maskani.>", "notifications-maskani", handler,
		nats.BindStream("maskani"),
		nats.Durable("notifications-maskani"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}

// dispatchMaskani queues the email and WhatsApp messages for one event. Idempotency keys are per
// event and channel: several payments or passes share one aggregate, so the aggregate id would drop
// all but the first, while a redelivered event never sends twice.
func dispatchMaskani(ctx context.Context, nc *nats.Conn, cfg *config.Config, ti *tenantInfo, tenantID string, evt eventslib.Event, mp maskaniMapping) error {
	p := evt.Payload
	data := mp.Data(p, ti)
	suffix := ""
	if mp.Path != nil {
		if path := strings.Trim(mp.Path(p), "/"); path != "" && ti.Slug != "" {
			suffix = ti.Slug + "/" + path
			app := ti.ServiceURL("maskani", "NOTIFICATIONS_MASKANI_APP_URL", maskaniButtonBase)
			data["action_link"] = strings.TrimRight(app, "/") + "/" + suffix
		}
	}
	target := messaging.TargetCustomer
	email, phone := mStr(p, mp.EmailKey), mStr(p, mp.PhoneKey)
	if mp.Staff {
		target, email, phone = messaging.TargetStaff, ti.ContactEmail, ti.ContactPhone
	}

	send := func(channel, to string, meta map[string]any) error {
		meta["service_id"] = "maskani"
		_, err := messaging.Publish(ctx, nc, cfg.Events, messaging.Message{
			TenantID:       tenantID,
			Channel:        channel,
			TemplateID:     mp.TemplateID,
			SenderScope:    messaging.SenderScopeTenant,
			Target:         target,
			To:             []string{to},
			Data:           data,
			Metadata:       meta,
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("maskani-%s-%s", evt.ID, channel),
			QueuedAt:       time.Now(),
		})
		return err
	}

	if email != "" && strings.Contains(email, "@") && !strings.HasSuffix(strings.ToLower(email), ".local") {
		if err := send("email", email, map[string]any{"subject": mp.Subject(p)}); err != nil {
			return err
		}
	}
	if phone != "" && mp.WATemplate != "" {
		meta := map[string]any{
			"template_name":     mp.WATemplate,
			"template_language": "en_US",
			"template_params":   mp.WAParams(data),
		}
		if strings.HasSuffix(mp.WATemplate, "_btn") {
			if suffix == "" {
				return nil // a button template cannot go without its link
			}
			meta["template_button_param"] = suffix
		}
		if code := dialCodeForCountry(ti.Country); code != "" {
			meta["default_dial_code"] = code
		} else {
			meta["default_dial_code"] = "254"
		}
		if err := send("whatsapp", phone, meta); err != nil {
			return err
		}
	}
	return nil
}
