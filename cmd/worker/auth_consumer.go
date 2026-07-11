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

// authUserEvent matches the payload published by auth-api's outbox for user events.
type authUserEvent struct {
	UserID     string `json:"user_id"`
	Email      string `json:"email"`
	FullName   string `json:"full_name"`
	Phone      string `json:"phone,omitempty"`
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Method     string `json:"method,omitempty"`
	// ServiceID identifies which service triggered this event (e.g. "truload", "logistics").
	// When set, the notifications-api resolves the tenant's per-service URL from service_urls
	// metadata instead of the generic app_url, so email links point to the right service.
	ServiceID  string `json:"service_id,omitempty"`
	// Admin-provisioned accounts (method="admin_provisioned"): the welcome email
	// links to the SSO sign-in page rather than a generic getting-started link.
	// The user signs in with a temporary password and must change it on first login.
	Welcome            bool   `json:"welcome,omitempty"`
	LoginURL           string `json:"login_url,omitempty"`
	MustChangePassword bool   `json:"must_change_password,omitempty"`
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
	welcomeHandler := func(m *nats.Msg) {
		// Auth outbox publishes the shared-events envelope; business fields live under
		// `payload` (like the reset/otp handlers below). Decoding m.Data straight into a
		// flat struct left every field empty → "no email, skipping" → welcome never sent.
		var envelope struct {
			TenantID   string         `json:"tenant_id"`
			TenantSlug string         `json:"tenant_slug"`
			Payload    map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(m.Data, &envelope); err != nil {
			logg.Error("auth user created: unmarshal failed", zap.Error(err))
			return
		}
		p := envelope.Payload
		if p == nil {
			logg.Warn("auth user created: no payload")
			return
		}
		var evt authUserEvent
		evt.UserID, _ = p["user_id"].(string)
		evt.Email, _ = p["email"].(string)
		evt.FullName, _ = p["full_name"].(string)
		evt.Phone, _ = p["phone"].(string)
		evt.Method, _ = p["method"].(string)
		evt.ServiceID, _ = p["service_id"].(string)
		evt.LoginURL, _ = p["login_url"].(string)
		evt.MustChangePassword, _ = p["must_change_password"].(bool)
		if evt.TenantID, _ = p["tenant_id"].(string); evt.TenantID == "" {
			evt.TenantID = envelope.TenantID
		}
		if evt.TenantSlug, _ = p["tenant_slug"].(string); evt.TenantSlug == "" {
			evt.TenantSlug = envelope.TenantSlug
		}

		if evt.Email == "" {
			logg.Warn("auth user created: no email, skipping", zap.String("user_id", evt.UserID))
			return
		}

		// Resolve tenant for branding and per-tenant app URL
		var tenantSlug string
		tenantAppURL := ""
		if evt.TenantID != "" {
			if ti, err := tr.resolve(ctx, evt.TenantID); err == nil {
				// Use per-service URL when service_id is set; fall back to tenant-level app_url.
				tenantAppURL = ti.ServiceURL(evt.ServiceID, "", ti.AppURL)
				tenantSlug = ti.Slug
			}
		}
		if tenantSlug == "" {
			tenantSlug = evt.TenantSlug
		}

		name := evt.FullName
		if name == "" {
			name = evt.Email
		}

		// Build getting_started_link: use resolved appURL+slug if available; renderMessage will also
		// auto-set it, but setting it here avoids relying on that fallback.
		gettingStartedLink := ""
		if tenantAppURL != "" && tenantSlug != "" {
			gettingStartedLink = tenantAppURL + "/" + tenantSlug
		} else if tenantAppURL != "" {
			gettingStartedLink = tenantAppURL
		}

		msgData := map[string]any{"name": name}
		if gettingStartedLink != "" {
			msgData["getting_started_link"] = gettingStartedLink
		}
		// Admin-provisioned accounts: surface the SSO login link + first-login
		// password-change notice so the template can render a "Sign In" CTA.
		if evt.LoginURL != "" {
			msgData["login_url"] = evt.LoginURL
		}
		if evt.MustChangePassword {
			msgData["must_change_password"] = true
		}

		subject := "Welcome to the platform!"
		if evt.Method == "admin_provisioned" {
			subject = "You've been added — set up your account"
		}
		metadata := map[string]any{"subject": subject}
		if evt.ServiceID != "" {
			metadata["service_id"] = evt.ServiceID
		}

		msg := messaging.Message{
			TenantID:    evt.TenantID,
			Channel:     "email",
			TemplateID:  "auth/welcome",
			SenderScope: messaging.SenderScopePlatform,
			Target:      messaging.TargetCustomer,
			To:          []string{evt.Email},
			Data:        msgData,
			Metadata:    metadata,
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
	}

	subscribeWithRetry(ctx, nil, logg, "auth notification consumer", true, func() (*nats.Subscription, error) {
		return eventslib.QueueSubscribe(logg, nc, "auth.user.created", "notif-welcome", welcomeHandler)
	})

	// Password reset email on reset request
	// Auth-api publishes auth.user.password_reset.requested via plain NATS (outbox → conn.Publish)
	resetHandler := func(m *nats.Msg) {
		// Auth outbox publishes shared-events.Event envelope
		var envelope struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(m.Data, &envelope); err != nil {
			logg.Error("auth password reset: unmarshal failed", zap.Error(err))
			return
		}

		payload := envelope.Payload
		if payload == nil {
			logg.Warn("auth password reset: no payload")
			return
		}

		email, _ := payload["email"].(string)
		if email == "" {
			logg.Warn("auth password reset: no email in payload")
			return
		}

		name, _ := payload["full_name"].(string)
		if name == "" {
			name = email
		}

		resetLink, _ := payload["reset_link"].(string)
		if resetLink == "" {
			resetLink = "https://accounts.codevertexitsolutions.com/reset-password"
		}

		tenantID, _ := payload["tenant_id"].(string)
		serviceID, _ := payload["service_id"].(string)

		resetMetadata := map[string]any{"subject": "Reset your password"}
		if serviceID != "" {
			resetMetadata["service_id"] = serviceID
		}

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  "auth/password_reset",
			SenderScope: messaging.SenderScopePlatform,
			Target:      messaging.TargetCustomer,
			To:          []string{email},
			Data: map[string]any{
				"name":       name,
				"reset_link": resetLink,
			},
			Metadata: resetMetadata,
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("auth-password-reset-%s", payload["user_id"]),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("auth password reset: failed to dispatch email", zap.String("email", email), zap.Error(err))
			return
		}

		logg.Info("password reset email dispatched", zap.String("to", email))
	}

	subscribeWithRetry(ctx, nil, logg, "auth password reset consumer", true, func() (*nats.Subscription, error) {
		return eventslib.QueueSubscribe(logg, nc, "auth.user.password_reset.requested", "notif-pwreset", resetHandler)
	})

	// OTP email — dispatched when user requests email verification before a helpdesk ticket
	otpHandler := func(m *nats.Msg) {
		var envelope struct {
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(m.Data, &envelope); err != nil {
			logg.Error("auth otp: unmarshal failed", zap.Error(err))
			return
		}

		payload := envelope.Payload
		if payload == nil {
			logg.Warn("auth otp: no payload")
			return
		}

		email, _ := payload["email"].(string)
		if email == "" {
			logg.Warn("auth otp: no email in payload")
			return
		}

		otp, _ := payload["otp"].(string)
		if otp == "" {
			logg.Warn("auth otp: no otp in payload")
			return
		}

		userID, _ := payload["user_id"].(string)
		tenantID, _ := payload["tenant_id"].(string)

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  "auth/otp_verification",
			SenderScope: messaging.SenderScopePlatform,
			Target:      messaging.TargetCustomer,
			To:          []string{email},
			Data: map[string]any{
				"name": email,
				"otp":  otp,
			},
			Metadata: map[string]any{
				"subject": "Your verification code",
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("auth-otp-%s-%d", userID, time.Now().Unix()/300),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("auth otp: failed to dispatch email", zap.String("email", email), zap.Error(err))
			return
		}

		logg.Info("OTP email dispatched", zap.String("to", email))
	}

	subscribeWithRetry(ctx, nil, logg, "auth otp consumer", true, func() (*nats.Subscription, error) {
		return eventslib.QueueSubscribe(logg, nc, "auth.user.otp.requested", "notif-otp", otpHandler)
	})
}
