package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/devicetoken"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// maskaniPush is a push sent to people's own devices (registered from the Maskani app) alongside
// the WhatsApp and email the mapping sends. A walk-in push is the fast path: it pops up on the
// host's phone and opens the page where they answer in one tap. A resident request pops up on the
// caretaker's and manager's phones and opens the work order.
var maskaniPush = map[string]struct {
	Template string
	Title    string
	Path     func(p map[string]any) string
	// Users picks the auth user ids to reach; nil means the payload's host_user_id.
	Users func(p map[string]any) []string
	Skip  func(p map[string]any) bool
}{
	"walk_in.requested": {Template: "maskani/walk_in_request", Title: "Visitor at the gate", Path: withID("portal/walk-ins", "event_id")},
	"visitor.arrived":   {Template: "maskani/visitor_arrived", Title: "Visitor arrived", Path: fixed("portal/visitors")},
	"work_order.created": {
		Template: "maskani/resident_request", Title: "New resident request", Path: withID("works", "work_order_id"), Users: responderIDs,
		Skip: func(p map[string]any) bool { return mStr(p, "source") != "resident" },
	},
	"billing.readings_missing": {
		Template: "maskani/readings_missing", Title: "Meter readings needed", Users: responderIDs,
		Path: func(p map[string]any) string {
			return "utilities/readings?property_id=" + mStr(p, "property_id") + "&period=" + mStr(p, "period")
		},
	},
	"manual_payment.submitted": {
		Template: "maskani/manual_payment", Title: "Payment to verify", Users: responderIDs, Path: fixed("collections?tab=verify"),
	},
	"adjustment.requested": {
		Template: "maskani/adjustment_requested", Title: "Credit to approve", Users: responderIDs, Path: fixed("collections?tab=credits"),
	},
	"payment_plan.broken": {
		Template: "maskani/payment_plan_broken", Title: "Payment plan broken", Users: responderIDs, Path: withID("billing/accounts", "account_id"),
	},
	"bill_query.raised": {
		Template: "maskani/bill_query_raised", Title: "Bill query", Users: responderIDs, Path: fixed("collections?tab=queries"),
	},
	"billing.run_ready": {
		Template: "maskani/run_ready", Title: "Bills ready to run", Users: responderIDs,
		Path: func(p map[string]any) string { return "billing/runs?property_id=" + mStr(p, "property_id") },
	},
}

// responderIDs are the auth user ids of a payload's responders.
func responderIDs(p map[string]any) []string {
	var ids []string
	for _, r := range maskaniResponders(p) {
		ids = append(ids, r.UserID)
	}
	return ids
}

// pushMaskaniHost sends the push for evt to the chosen users' active devices in the tenant. A ring
// (the guard asked again) gets its own idempotency key per ring so it is not dropped as a repeat.
func pushMaskaniHost(ctx context.Context, nc *nats.Conn, cfg *config.Config, client *ent.Client, ti *tenantInfo, evt eventslib.Event, log *zap.Logger) {
	spec, ok := maskaniPush[evt.EventType]
	if !ok || client == nil || (spec.Skip != nil && spec.Skip(evt.Payload)) {
		return
	}
	raw := []string{mStr(evt.Payload, "host_user_id")}
	if spec.Users != nil {
		raw = spec.Users(evt.Payload)
	}
	users := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		if id, err := uuid.Parse(s); err == nil {
			users = append(users, id)
		}
	}
	if len(users) == 0 {
		log.Info("maskani push: no user to reach", zap.String("type", evt.EventType))
		return
	}
	tokens, err := client.DeviceToken.Query().
		Where(devicetoken.TenantID(evt.TenantID), devicetoken.UserIDIn(users...), devicetoken.IsActive(true)).All(ctx)
	if err != nil {
		log.Warn("maskani push: device token lookup failed", zap.String("type", evt.EventType), zap.Error(err))
		return
	}
	if len(tokens) == 0 {
		// The person has not turned alerts on in the app on any device: WhatsApp and email still went.
		log.Info("maskani push: no registered device", zap.String("type", evt.EventType), zap.Int("users", len(users)))
		return
	}
	toks := make([]string, 0, len(tokens))
	for _, t := range tokens {
		toks = append(toks, t.Token)
	}
	data := map[string]any{"visitor_name": waParam(evt.Payload["visitor_name"], "A visitor"), "unit_code": mStr(evt.Payload, "unit_code"),
		"title": mStr(evt.Payload, "title"), "property": mStr(evt.Payload, "property"), "missing_count": mStr(evt.Payload, "missing_count"),
		"account_ref": mStr(evt.Payload, "account_ref"), "method": strings.ReplaceAll(mStr(evt.Payload, "method"), "_", " "),
		"type": "maskani_" + evt.EventType}
	if mStr(evt.Payload, "amount") != "" {
		data["amount"] = mMoney(evt.Payload, "amount")
	}
	if per := mStr(evt.Payload, "period"); per != "" {
		data["period"] = maskaniPeriod(per)
	}
	if path := spec.Path(evt.Payload); path != "" && ti.Slug != "" {
		data["url"] = "/" + ti.Slug + "/" + path // opens inside the app when tapped
	}
	if evt.Payload["ring"] == true {
		data["ring"] = "true"
	}
	key := fmt.Sprintf("maskani-%s-push", evt.ID)
	msg := messaging.Message{
		TenantID:       evt.TenantID.String(),
		Channel:        "push",
		TemplateID:     spec.Template,
		SenderScope:    messaging.SenderScopeTenant,
		Target:         messaging.TargetCustomer,
		To:             messaging.NormalizeRecipients(toks, "push"),
		Data:           data,
		Metadata:       map[string]any{"push_title": spec.Title, "service_id": "maskani", "urgent": evt.EventType == "walk_in.requested"},
		RequestID:      uuid.New().String(),
		IdempotencyKey: key,
		QueuedAt:       time.Now(),
	}
	if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
		log.Warn("maskani push: publish failed", zap.String("type", evt.EventType), zap.Error(err))
		return
	}
	log.Info("maskani push queued", zap.String("type", evt.EventType), zap.Int("devices", len(toks)))
}
