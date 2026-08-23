package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// startResellerApplicationNotificationConsumer subscribes to
// auth.reseller_application.{approved,status_updated} (auth-api, reseller_handler.go,
// shipped 2026-08-23 — see .claude/plans/reseller-partner-program-plan-2026-08-23.md §7/N1)
// and dispatches the applicant-facing decision email.
//
// Unlike the tenant-approval flow this mirrors (startAuthNotificationConsumer's
// tenantDecisionHandler, which needs a SEPARATE dedicated auth.tenant.{approved,rejected}.
// notify event because the generic lifecycle event carries no email/name), the reseller
// application event ALREADY carries contact_email and business_name directly in its own
// payload (resellerApplicationEventPayload, reseller_handler.go) — so this subscribes
// straight to the real domain events with no new auth-api-side plumbing needed.
func startResellerApplicationNotificationConsumer(ctx context.Context, nc *nats.Conn, cfg *config.Config, logg *zap.Logger) {
	if nc == nil {
		logg.Warn("skipping reseller application notification consumer: NATS not available")
		return
	}

	send := func(templateID, defaultSubject string, payload map[string]any) {
		msg, ok := buildResellerDecisionMessage(templateID, defaultSubject, payload)
		if !ok {
			logg.Warn("reseller application decision: no contact_email in payload",
				zap.String("template", templateID))
			return
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("reseller application decision: failed to dispatch email",
				zap.String("template", templateID), zap.String("email", msg.To[0]), zap.Error(err))
			return
		}
		logg.Info("reseller application decision email dispatched",
			zap.String("template", templateID), zap.String("to", msg.To[0]))
	}

	approvedHandler := func(m *nats.Msg) {
		var evt eventslib.Event
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("reseller_application.approved: unmarshal failed", zap.Error(err))
			return
		}
		if evt.Payload == nil {
			logg.Warn("reseller_application.approved: no payload")
			return
		}
		send("auth/reseller_approved", "You're a Certified Codevertex Reseller Partner", evt.Payload)
	}

	statusUpdatedHandler := func(m *nats.Msg) {
		var evt eventslib.Event
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("reseller_application.status_updated: unmarshal failed", zap.Error(err))
			return
		}
		if evt.Payload == nil {
			logg.Warn("reseller_application.status_updated: no payload")
			return
		}
		status, _ := evt.Payload["status"].(string)
		if !shouldEmailOnStatusUpdate(status) {
			return
		}
		send("auth/reseller_rejected", "Update on your Certified Reseller Program application", evt.Payload)
	}

	subscribeWithRetry(ctx, nil, logg, "reseller application approved consumer", true, func() (*nats.Subscription, error) {
		return eventslib.QueueSubscribe(logg, nc, "auth.reseller_application.approved", "notif-reseller-approved", approvedHandler)
	})

	subscribeWithRetry(ctx, nil, logg, "reseller application status_updated consumer", true, func() (*nats.Subscription, error) {
		return eventslib.QueueSubscribe(logg, nc, "auth.reseller_application.status_updated", "notif-reseller-status-updated", statusUpdatedHandler)
	})
}

// shouldEmailOnStatusUpdate reports whether a status_updated event's target status warrants
// an applicant-facing email. status_updated fires for EVERY stage transition (pending ->
// kyb_pending -> kyb_approved -> agreement_pending too, not just terminal outcomes) — only the
// one terminal-but-not-approved outcome ("rejected") gets an email here; "approved" has its
// own dedicated event/handler, and every other intermediate stage is deliberately silent (an
// admin-facing progress detail, not something the applicant needs an email per stage for).
func shouldEmailOnStatusUpdate(status string) bool {
	return status == "rejected"
}

// buildResellerDecisionMessage builds the outbound email Message from a reseller_application
// event payload. Pure (no ctx/NATS/DB access) so it's unit-testable without a broker — the
// caller (send, above) does the actual dispatch. Returns ok=false when the payload has no
// usable contact_email, so the caller can skip without constructing an unsendable message.
func buildResellerDecisionMessage(templateID, defaultSubject string, payload map[string]any) (messaging.Message, bool) {
	email, _ := payload["contact_email"].(string)
	if email == "" {
		return messaging.Message{}, false
	}
	businessName, _ := payload["business_name"].(string)
	if businessName == "" {
		businessName = email
	}
	tenantID, _ := payload["tenant_id"].(string)
	requestedTier, _ := payload["requested_tier"].(string)
	applicationID, _ := payload["application_id"].(string)

	data := map[string]any{"name": businessName, "business_name": businessName, "requested_tier": requestedTier}

	return messaging.Message{
		TenantID:       tenantID,
		Channel:        "email",
		TemplateID:     templateID,
		SenderScope:    messaging.SenderScopePlatform,
		Target:         messaging.TargetCustomer,
		To:             []string{email},
		Data:           data,
		Metadata:       map[string]any{"subject": defaultSubject},
		RequestID:      uuid.New().String(),
		IdempotencyKey: fmt.Sprintf("reseller-application-%s-%s", templateID, applicationID),
		QueuedAt:       time.Now(),
	}, true
}
