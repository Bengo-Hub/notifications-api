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

// startLegalChangeNoticeConsumer emails a user about a material change to the Terms of Service or
// the Privacy Policy (owner 2026-11-15). auth-api publishes one auth.legal.updated.notify event
// per user when such a version is published from its dashboard; the idempotency key is per user
// and version, so a repeated fan-out never mails anyone twice.
func startLegalChangeNoticeConsumer(ctx context.Context, nc *nats.Conn, cfg *config.Config, logg *zap.Logger) {
	if nc == nil {
		logg.Warn("skipping legal change notice consumer: NATS not available")
		return
	}
	handler := func(m *nats.Msg) {
		var envelope struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(m.Data, &envelope); err != nil {
			logg.Error("legal change notice: unmarshal failed", zap.Error(err))
			return
		}
		msg, ok := buildLegalChangeMessage(envelope.Payload, time.Now())
		if !ok {
			logg.Warn("legal change notice: payload without an email, user or version")
			return
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("legal change notice: failed to dispatch email", zap.Strings("to", msg.To), zap.Error(err))
		}
	}
	subscribeWithRetry(ctx, nil, logg, "auth legal change notice consumer", true, func() (*nats.Subscription, error) {
		return eventslib.QueueSubscribe(logg, nc, "auth.legal.updated.notify", "notif-legal-updated", handler)
	})
}

// buildLegalChangeMessage turns one event payload into the email. ok is false when the payload
// lacks what the email needs.
func buildLegalChangeMessage(p map[string]any, now time.Time) (messaging.Message, bool) {
	str := func(k string) string { s, _ := p[k].(string); return s }
	email, userID, version, title := str("email"), str("user_id"), str("version"), str("title")
	if email == "" || userID == "" || version == "" {
		return messaging.Message{}, false
	}
	if title == "" {
		title = "Terms"
	}
	name := str("name")
	if name == "" {
		name = email
	}
	inForce, _ := p["in_force"].(bool)
	data := map[string]any{
		"name":           name,
		"title":          title,
		"version":        version,
		"summary":        str("summary"),
		"effective_date": str("effective_date"),
		"in_force":       inForce,
		"document_url":   str("document_url"),
	}
	subject := fmt.Sprintf("We are updating our %s", title)
	if inForce {
		subject = fmt.Sprintf("We have updated our %s", title)
	}
	return messaging.Message{
		Channel:        "email",
		TemplateID:     "auth/legal_updated",
		SenderScope:    messaging.SenderScopePlatform,
		Target:         messaging.TargetCustomer,
		To:             []string{email},
		Data:           data,
		Metadata:       map[string]any{"subject": subject},
		RequestID:      uuid.New().String(),
		IdempotencyKey: fmt.Sprintf("auth-legal-%s-%s-%s", str("doc_type"), version, userID),
		QueuedAt:       now,
	}, true
}
