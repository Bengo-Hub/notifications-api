package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// Maskani (property platform) notifications. maskani-api publishes maskani.<event_type> through
// its outbox; owners, occupants, visitors and buyers get SMS (they often have no email), and
// urgent estate alerts go to the tenant contact phone. Notices to an audience are sent by
// maskani-api directly through the send API, not from events.

type maskaniMapping struct {
	TemplateID string
	Target     string
	// Recipient returns the phone to send to; staff alerts fall back to the tenant contact phone.
	Recipient func(payload map[string]any, ti *tenantInfo) (string, bool)
	Data      func(payload map[string]any, portal string) map[string]any
	// Skip lets a mapping ignore events it should not message about.
	Skip func(payload map[string]any) bool
}

func maskaniPayloadPhone(key string) func(map[string]any, *tenantInfo) (string, bool) {
	return func(p map[string]any, _ *tenantInfo) (string, bool) { return recipientFromPayload(p, key) }
}

func maskaniContactPhone(_ map[string]any, ti *tenantInfo) (string, bool) {
	return ti.ContactPhone, ti.ContactPhone != ""
}

func maskaniStr(p map[string]any, key string) string {
	v, _ := p[key].(string)
	return v
}

// maskaniTime renders an RFC3339 payload time as "7 Oct 15:04" for a short SMS.
func maskaniTime(p map[string]any, key string) string {
	s := maskaniStr(p, key)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.Format("2 Jan 15:04")
	}
	return s
}

var maskaniMappings = map[string]maskaniMapping{
	"bill.issued": {
		TemplateID: "maskani/bill_issued", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("phone"),
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"name": p["name"], "period": p["period"], "account_ref": p["account_ref"],
				"amount": p["amount"], "due_date": p["due_date"], "paybill": p["paybill"]}
		},
	},
	"payment.applied": {
		TemplateID: "maskani/payment_received", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("phone"),
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"amount": p["amount"], "account_ref": p["account_ref"], "receipt": p["receipt"], "balance": p["balance"]}
		},
	},
	"pass.created": {
		TemplateID: "maskani/visitor_pass", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("visitor_phone"),
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"visitor_name": p["visitor_name"], "code": p["code"],
				"valid_from": maskaniTime(p, "valid_from"), "valid_to": maskaniTime(p, "valid_to")}
		},
	},
	"visitor.arrived": {
		TemplateID: "maskani/visitor_arrived", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("host_phone"),
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"visitor_name": p["visitor_name"], "unit_code": p["unit_code"], "time": maskaniTime(p, "occurred_at")}
		},
	},
	"walk_in.requested": {
		TemplateID: "maskani/walk_in_request", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("host_phone"),
		Data: func(p map[string]any, portal string) map[string]any {
			return map[string]any{"visitor_name": p["visitor_name"], "unit_code": p["unit_code"],
				"decide_link": portal + "/walk-ins/" + maskaniStr(p, "event_id")}
		},
	},
	"party.invited": {
		TemplateID: "maskani/portal_invite", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("phone"),
		Data: func(p map[string]any, portal string) map[string]any {
			link := maskaniStr(p, "portal_url")
			if link == "" {
				link = portal
			}
			return map[string]any{"name": p["name"], "portal_link": link}
		},
	},
	"sale_contract.activated": {
		TemplateID: "maskani/contract_activated", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("phone"),
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"name": p["buyer"], "contract_number": p["contract_number"], "unit_code": p["unit_code"], "account_ref": p["account_ref"]}
		},
	},
	"sale_contract.fully_paid": {
		TemplateID: "maskani/contract_fully_paid", Target: messaging.TargetCustomer, Recipient: maskaniPayloadPhone("phone"),
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"contract_number": p["contract_number"], "account_ref": p["account_ref"]}
		},
	},
	"incident.reported": {
		TemplateID: "maskani/incident_alert", Target: messaging.TargetStaff, Recipient: maskaniContactPhone,
		Skip: func(p map[string]any) bool { urgent, _ := p["urgent"].(bool); return !urgent },
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"number": p["number"], "severity": p["severity"], "title": p["title"]}
		},
	},
	"work_order.sla_breached": {
		TemplateID: "maskani/work_order_sla_breached", Target: messaging.TargetStaff, Recipient: maskaniContactPhone,
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"number": p["number"], "title": p["title"], "priority": p["priority"]}
		},
	},
	"vendor.document_expiring": {
		TemplateID: "maskani/vendor_document_expiring", Target: messaging.TargetStaff, Recipient: maskaniContactPhone,
		Data: func(p map[string]any, _ string) map[string]any {
			return map[string]any{"vendor": p["vendor"], "doc_type": p["doc_type"], "days_left": p["days_left"]}
		},
	},
}

// startMaskaniConsumer subscribes to maskani.> and sends the mapped SMS.
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
		mapping, ok := maskaniMappings[evt.EventType]
		if !ok || (mapping.Skip != nil && mapping.Skip(evt.Payload)) {
			_ = m.Ack()
			return
		}
		if evt.TenantID == uuid.Nil {
			log.Warn("maskani event: no tenant_id, skipping", zap.String("type", evt.EventType))
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
		to, ok := mapping.Recipient(evt.Payload, ti)
		if !ok {
			_ = m.Ack()
			return
		}
		app := ti.ServiceURL("maskani", "NOTIFICATIONS_MASKANI_APP_URL", "https://maskaniapp.codevertexafrica.com")
		portal := strings.TrimRight(app, "/") + "/" + ti.Slug + "/portal"

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "sms",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      mapping.Target,
			To:          []string{to},
			Data:        mapping.Data(evt.Payload, portal),
			Metadata:    map[string]any{"service_id": "maskani"},
			RequestID:   uuid.New().String(),
			// One message per event: several payments or passes share an aggregate, so the
			// aggregate id would drop all but the first.
			IdempotencyKey: fmt.Sprintf("maskani-%s", evt.ID),
			QueuedAt:       time.Now(),
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
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
