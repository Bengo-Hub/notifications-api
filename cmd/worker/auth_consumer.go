package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// authUserEvent matches the payload published by auth-api's outbox for user events.
type authUserEvent struct {
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	FullName   string `json:"full_name"`
	Phone      string `json:"phone,omitempty"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Method     string `json:"method,omitempty"`
}

// startAuthNotificationConsumer subscribes to auth.user.created events (plain NATS,
// not JetStream) and dispatches welcome emails for new user registrations.
// This runs alongside the identity sync consumer (in the API process) without conflict
// because plain NATS subscriptions deliver to all subscribers.
func startAuthNotificationConsumer(ctx context.Context, nc *nats.Conn, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil {
		logg.Warn("skipping auth notification consumer: NATS not available")
		return
	}

	// Welcome email on user registration
	_, err := nc.Subscribe("auth.user.created", func(m *nats.Msg) {
		var evt authUserEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("auth user created: unmarshal failed", zap.Error(err))
			return
		}

		if evt.Email == "" {
			logg.Warn("auth user created: no email, skipping", zap.String("user_id", evt.UserID))
			return
		}

		// Resolve tenant for branding
		tenantWebsite := ""
		if evt.TenantID != "" {
			if ti, err := tr.resolve(ctx, evt.TenantID); err == nil {
				tenantWebsite = ti.Website
			}
		}
		if tenantWebsite == "" {
			tenantWebsite = "https://accounts.codevertexitsolutions.com"
		}

		name := evt.FullName
		if name == "" {
			name = evt.Email
		}

		msg := messaging.Message{
			TenantID:   evt.TenantID,
			Channel:    "email",
			TemplateID: "auth/welcome",
			To:         []string{evt.Email},
			Data: map[string]any{
				"name":                 name,
				"getting_started_link": fmt.Sprintf("%s/dashboard", tenantWebsite),
			},
			Metadata: map[string]any{
				"subject": "Welcome to the platform!",
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("auth-welcome-%s", evt.UserID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("auth user created: failed to dispatch welcome email",
				zap.String("user_id", evt.UserID),
				zap.String("email", evt.Email),
				zap.Error(err),
			)
			return
		}

		logg.Info("welcome email dispatched",
			zap.String("user_id", evt.UserID),
			zap.String("to", evt.Email),
		)
	})
	if err != nil {
		logg.Warn("auth notification consumer subscription failed", zap.Error(err))
		return
	}

	logg.Info("auth notification consumer started", zap.String("subject", "auth.user.created"))
}
