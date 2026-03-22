package main

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"fmt"

	"github.com/google/uuid"

	entdb "github.com/bengobox/notifications-api/internal/database"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/platform/database"
	"github.com/bengobox/notifications-api/internal/platform/events"
	"github.com/bengobox/notifications-api/internal/platform/templates"
	"github.com/bengobox/notifications-api/internal/providers"
	"github.com/bengobox/notifications-api/internal/shared/logger"

	"github.com/bengobox/notifications-api/internal/modules/billing"
	"github.com/bengobox/notifications-api/internal/modules/tenant"
	"github.com/Bengo-Hub/shared-service-client"
)

const maxRetries = 3

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}
	logg, err := logger.New(cfg.App.Env)
	if err != nil {
		log.Fatalf("logger init failed: %v", err)
	}

	nc, err := events.Connect(cfg.Events)
	if err != nil {
		logg.Fatal("nats connect failed", zap.Error(err))
	}
	js, err := nc.JetStream()
	if err != nil {
		logg.Fatal("jetstream init failed", zap.Error(err))
	}
	subject := cfg.Events.Subject
	if subject == "" {
		subject = "notifications.events"
	}
	// Ensure stream exists
	if _, err := js.StreamInfo(cfg.Events.StreamName); err != nil {
		if _, err := js.AddStream(&nats.StreamConfig{
			Name:      cfg.Events.StreamName,
			Subjects:  []string{subject},
			Retention: nats.LimitsPolicy,
			Storage:   nats.FileStorage,
			MaxAge:    7 * 24 * time.Hour,
		}); err != nil {
			logg.Fatal("stream ensure failed", zap.Error(err))
		}
	}

	tpl := templates.New(cfg.Templates)

	// DB for provider overrides and billing
	client, err := entdb.NewClient(ctx, cfg.Postgres)
	if err != nil {
		logg.Fatal("failed to connect to ent", zap.Error(err))
	}
	defer client.Close()

	if err := client.Schema.Create(ctx); err != nil {
		logg.Fatal("failed to create schema", zap.Error(err))
	}

	// Initialize Treasury Client for top-up monitoring
	treasuryCfg := serviceclient.DefaultConfig(cfg.Services.TreasuryAPI, "treasury-api", logg)
	treasuryClient := serviceclient.New(treasuryCfg)

	billingSvc := billing.NewService(client, logg, treasuryClient)
	dbPool, err := database.NewPool(ctx, cfg.Postgres)
	if err != nil {
		logg.Warn("postgres not available for provider overrides", zap.Error(err))
	}

	// Sync platform owner tenant
	tenantSyncer := tenant.NewSyncer(client, cfg.Services.AuthAPI)
	platformID, err := tenantSyncer.SyncTenant(ctx, "codevertex")
	if err != nil {
		logg.Warn("failed to sync platform owner, using fallback", zap.Error(err))
	}
	platformIDStr := platformID.String()

	pm := providers.NewManager(dbPool, cfg.Postgres, cfg.Providers, encryption.KeyFromEnv(cfg.Security.EncryptionKey), cfg.App.Env, platformIDStr)

	durable := "notifications-worker"
	// Tenant resolver for event consumers and template branding
	tr := newTenantResolver(client)

	_, err = js.Subscribe(subject, func(m *nats.Msg) {
		var msg messaging.Message
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			logg.Error("invalid message, dropping", zap.Error(err))
			_ = m.Ack() // unrecoverable — don't retry
			return
		}

		// Determine retry attempt from NATS metadata
		meta, _ := m.Metadata()
		attempt := uint64(1)
		if meta != nil {
			attempt = meta.NumDelivered
		}

		// Render template
		rendered, renderErr := renderMessage(ctx, cfg, tpl, tr, &msg, logg)
		if renderErr != nil {
			logg.Error("template render failed, dropping", zap.String("template", msg.TemplateID), zap.Error(renderErr))
			_ = m.Ack() // template errors are not transient
			return
		}

		// Deliver via provider
		deliverErr := deliver(ctx, cfg, pm, billingSvc, &msg, rendered, logg)
		if deliverErr != nil {
			logg.Warn("delivery failed",
				zap.String("channel", msg.Channel),
				zap.String("template", msg.TemplateID),
				zap.Uint64("attempt", attempt),
				zap.Error(deliverErr),
			)

			if attempt >= maxRetries {
				logg.Error("max retries exceeded, dropping message",
					zap.String("channel", msg.Channel),
					zap.String("tenant_id", msg.TenantID),
					zap.String("request_id", msg.RequestID),
					zap.Strings("to", msg.To),
					zap.Uint64("attempts", attempt),
					zap.Error(deliverErr),
				)
				_ = m.Ack() // give up after max retries
			} else {
				// NAck triggers redelivery after AckWait (30s)
				_ = m.Nak()
			}
			return
		}

		logg.Info("message delivered",
			zap.String("channel", msg.Channel),
			zap.String("template", msg.TemplateID),
			zap.Strings("to", msg.To),
			zap.Uint64("attempt", attempt),
		)
		_ = m.Ack()
	}, nats.Durable(durable), nats.ManualAck(), nats.AckWait(30*time.Second), nats.MaxDeliver(maxRetries))
	if err != nil {
		logg.Fatal("subscription failed", zap.Error(err))
	}

	// Start fleet lifecycle event consumer (logistics-service → email notifications)
	startFleetConsumer(ctx, nc, js, cfg, tr, logg)

	// Start inventory stock event consumer (inventory-service → low stock alerts)
	startInventoryConsumer(ctx, nc, js, cfg, tr, logg)

	// Start order status event consumer (ordering-service → customer notifications)
	startOrderConsumer(ctx, nc, js, cfg, tr, logg)

	<-ctx.Done()
	_ = nc.Drain()
}

// renderMessage loads the template, renders it with tenant branding from DB, and returns the rendered content.
func renderMessage(ctx context.Context, cfg *config.Config, tpl *templates.Loader, tr *tenantResolver, msg *messaging.Message, logg *zap.Logger) (string, error) {
	// Always prepend channel to template ID for the loader (e.g. "email/auth/test_email")
	tplID := msg.Channel + "/" + msg.TemplateID
	content, err := tpl.Get(ctx, tplID)
	if err != nil {
		return "", err
	}

	var rendered strings.Builder

	if msg.Channel == "email" {
		basePath := filepath.Join(cfg.Templates.Directory, "email", "shared", "base.html")
		baseBytes, readErr := os.ReadFile(basePath)
		if readErr != nil {
			logg.Warn("base template not found", zap.String("base", basePath), zap.Error(readErr))
		}

		data := map[string]any{}
		for k, v := range msg.Data {
			data[k] = v
		}

		// Load tenant branding from DB
		if t, err := tr.resolveWithBranding(ctx, msg.TenantID); err == nil {
			if t.Name != "" {
				data["brand_name"] = t.Name
				data["brand"] = t.Name
			}
			if t.ContactEmail != "" {
				data["brand_email"] = t.ContactEmail
			}
			if t.ContactPhone != "" {
				data["brand_phone"] = t.ContactPhone
			}
			if t.LogoURL != "" {
				data["brand_logo_url"] = t.LogoURL
			}
			if t.PrimaryColor != "" {
				data["brand_primary_color"] = t.PrimaryColor
			}
			if t.SecondaryColor != "" {
				data["brand_secondary_color"] = t.SecondaryColor
			}
		} else {
			logg.Warn("failed to load tenant branding", zap.String("tenant_id", msg.TenantID), zap.Error(err))
		}

		if _, ok := data["brand_name"]; !ok || data["brand_name"] == "" {
			data["brand_name"] = "Notifications"
		}

		tplSet := template.New("base")
		if len(baseBytes) > 0 {
			if _, err := tplSet.Parse(string(baseBytes)); err != nil {
				logg.Warn("base template parse failed", zap.Error(err))
			}
		}
		if _, err := tplSet.Parse(content); err != nil {
			return "", err
		}
		if err := tplSet.Execute(&rendered, data); err != nil {
			_ = tplSet.ExecuteTemplate(&rendered, "content", data)
		}
	} else {
		t, err := template.New("msg").Parse(content)
		if err != nil {
			return "", err
		}
		if err := t.Execute(&rendered, msg.Data); err != nil {
			return "", err
		}
	}

	return rendered.String(), nil
}

// deliver sends the rendered message via the appropriate provider.
func deliver(ctx context.Context, cfg *config.Config, pm *providers.Manager, billingSvc *billing.Service, msg *messaging.Message, rendered string, logg *zap.Logger) error {
	channel := strings.ToLower(msg.Channel)
	tenantID, _ := uuid.Parse(msg.TenantID)
	preferred := ""
	if p, ok := msg.Metadata["provider"].(string); ok {
		preferred = p
	}

	switch channel {
	case "email":
		subject := "Notification"
		if s, ok := msg.Metadata["subject"].(string); ok && s != "" {
			subject = s
		}
		emailProv, _ := pm.GetEmailProvider(ctx, msg.TenantID, preferred)
		// Pass empty from — let the provider use its configured from address (tenant or platform fallback)
		if err := emailProv.SendEmail(ctx, "", msg.To, subject, rendered, ""); err != nil {
			return err
		}
		logg.Info("email sent", zap.String("provider", emailProv.Name()), zap.String("template", msg.TemplateID), zap.Strings("to", msg.To))
		return nil

	case "sms":
		// Deduct credits based on segments and recipient count
		if err := billingSvc.DeductSMSCredits(ctx, tenantID, rendered, len(msg.To), "SMS Delivery"); err != nil {
			return fmt.Errorf("billing: %w", err)
		}

		smsProv, _ := pm.GetSMSProvider(ctx, msg.TenantID, preferred)
		if err := smsProv.SendSMS(ctx, cfg.Providers.DefaultSMSSender, msg.To, rendered); err != nil {
			return err
		}
		logg.Info("sms sent", zap.String("provider", smsProv.Name()), zap.Strings("to", msg.To))
		return nil

	case "whatsapp":
		if err := billingSvc.DeductWhatsAppCredits(ctx, tenantID, len(msg.To), "WhatsApp Delivery"); err != nil {
			return fmt.Errorf("billing: %w", err)
		}

		waProv, err := pm.GetWhatsAppProvider(ctx, msg.TenantID, preferred)
		if err != nil {
			return err
		}
		
		waMetadata := make(map[string]interface{})
		for k, v := range msg.Metadata {
			waMetadata[k] = v
		}

		if err := waProv.SendWhatsApp(ctx, cfg.Providers.DefaultSMSSender, msg.To, rendered, waMetadata); err != nil {
			return err
		}
		logg.Info("whatsapp message sent", zap.String("provider", waProv.Name()), zap.Strings("to", msg.To))
		return nil

	case "push":
		pushProv, err := pm.GetPushProvider(ctx)
		if err != nil {
			logg.Warn("push provider unavailable", zap.Error(err))
			return nil // non-fatal: FCM may not be configured in all envs
		}
		title, _ := msg.Metadata["push_title"].(string)
		pushData := make(map[string]string)
		for k, v := range msg.Data {
			if s, ok := v.(string); ok {
				pushData[k] = s
			}
		}
		if err := pushProv.SendPush(ctx, msg.To, title, rendered, pushData); err != nil {
			return err
		}
		logg.Info("push notification sent", zap.String("provider", pushProv.Name()), zap.Strings("to", msg.To))
		return nil

	default:
		logg.Warn("unknown channel", zap.String("channel", msg.Channel))
		return nil
	}
}
