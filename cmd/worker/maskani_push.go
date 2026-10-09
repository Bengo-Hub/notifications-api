package main

import (
	"context"
	"fmt"
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

// maskaniPush is a push sent to the host's own devices (registered from the Maskani portal) for gate
// events, alongside the WhatsApp and email the mapping sends. A walk-in push is the fast path: it
// pops up on the host's phone and opens the page where they answer in one tap.
var maskaniPush = map[string]struct {
	Template string
	Title    string
	Path     func(p map[string]any) string
}{
	"walk_in.requested": {Template: "maskani/walk_in_request", Title: "Visitor at the gate", Path: withID("portal/walk-ins", "event_id")},
	"visitor.arrived":   {Template: "maskani/visitor_arrived", Title: "Visitor arrived", Path: fixed("portal/visitors")},
}

// pushMaskaniHost sends the push for evt to host_user_id's active devices in the tenant. A ring (the
// guard asked again) gets its own idempotency key per ring so it is not dropped as a repeat.
func pushMaskaniHost(ctx context.Context, nc *nats.Conn, cfg *config.Config, client *ent.Client, ti *tenantInfo, evt eventslib.Event, log *zap.Logger) {
	spec, ok := maskaniPush[evt.EventType]
	if !ok || client == nil {
		return
	}
	userID, err := uuid.Parse(mStr(evt.Payload, "host_user_id"))
	if err != nil {
		return
	}
	tokens, err := client.DeviceToken.Query().
		Where(devicetoken.TenantID(evt.TenantID), devicetoken.UserID(userID), devicetoken.IsActive(true)).All(ctx)
	if err != nil {
		log.Warn("maskani push: device token lookup failed", zap.String("type", evt.EventType), zap.Error(err))
		return
	}
	if len(tokens) == 0 {
		return
	}
	toks := make([]string, 0, len(tokens))
	for _, t := range tokens {
		toks = append(toks, t.Token)
	}
	data := map[string]any{"visitor_name": waParam(evt.Payload["visitor_name"], "A visitor"), "unit_code": mStr(evt.Payload, "unit_code"),
		"type": "maskani_" + evt.EventType}
	if path := spec.Path(evt.Payload); path != "" && ti.Slug != "" {
		data["url"] = "/" + ti.Slug + "/" + path // opens inside the portal when tapped
	}
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
		IdempotencyKey: fmt.Sprintf("maskani-%s-push", evt.ID),
		QueuedAt:       time.Now(),
	}
	if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
		log.Warn("maskani push: publish failed", zap.String("type", evt.EventType), zap.Error(err))
	}
}
