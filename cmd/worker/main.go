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

	sharedcache "github.com/Bengo-Hub/cache"
	serviceclient "github.com/Bengo-Hub/shared-service-client"

	entdb "github.com/bengobox/notifications-api/internal/database"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/platform/cache"
	"github.com/bengobox/notifications-api/internal/platform/database"
	"github.com/bengobox/notifications-api/internal/platform/events"
	"github.com/bengobox/notifications-api/internal/platform/templates"
	"github.com/bengobox/notifications-api/internal/providers"
	"github.com/bengobox/notifications-api/internal/providers/email"
	"github.com/bengobox/notifications-api/internal/shared/logger"

	"github.com/bengobox/notifications-api/internal/modules/billing"
	"github.com/bengobox/notifications-api/internal/modules/tenant"
)

// maxDeliveryAttempts caps how many times the worker tries to DELIVER a single
// notification before dead-lettering it. One attempt only: the deliver() path
// already falls back across providers (tenant SMTP -> platform SMTP) within that
// single attempt, so a failure means every fallback option was already exhausted.
// Redelivering would just hammer a rate-limited or blocked provider (e.g. Zoho
// "550 5.4.6 Unusual sending activity detected"), making the block worse — so we
// drop after one attempt rather than retrying the same failing send.
const maxDeliveryAttempts = 1

// subscribeWithRetry attempts a JetStream subscription with exponential backoff.
// After a pod restart, NATS may still consider the previous durable consumer
// binding active until its heartbeat expires. This retries until it succeeds.
// If background is true, runs the retry loop in a goroutine and returns immediately.
func subscribeWithRetry(ctx context.Context, js nats.JetStreamContext, logg *zap.Logger, name string, background bool, subscribeFn func() (*nats.Subscription, error)) {
	const maxAttempts = 12
	doSubscribe := func() {
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			_, err := subscribeFn()
			if err == nil {
				logg.Info(name+" subscription established")
				return
			}
			if attempt == maxAttempts {
				logg.Error(name+" subscription failed after retries", zap.Int("attempts", maxAttempts), zap.Error(err))
				return
			}
			backoff := time.Duration(attempt) * 5 * time.Second
			logg.Warn(name+" subscription failed, retrying", zap.Int("attempt", attempt), zap.Duration("backoff", backoff), zap.Error(err))
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}
	}
	if background {
		go doSubscribe()
	} else {
		doSubscribe()
	}
}

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
	whatsappSubsSvc := billing.NewWhatsAppSubscriptionService(client, logg, treasuryClient)
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
	// Redis for cached auth-api tenant data (branding, contact info)
	redisClient := cache.NewClient(cfg.Redis)

	// Shared cache-aside helper for tenant branding (auto-fetches from auth-api on miss)
	tenantCache := sharedcache.New(redisClient, logg)

	// Tenant resolver for event consumers and template branding
	tr := newTenantResolver(client, tenantCache, cfg.Services.AuthAPI)

	msgHandler := func(m *nats.Msg) {
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
		deliverErr := deliver(ctx, cfg, pm, billingSvc, tr, &msg, rendered, logg)
		if deliverErr != nil {
			logg.Warn("delivery failed",
				zap.String("channel", msg.Channel),
				zap.String("template", msg.TemplateID),
				zap.Uint64("attempt", attempt),
				zap.Error(deliverErr),
			)

			if attempt >= maxDeliveryAttempts {
				logg.Error("delivery failed, dropping (single-attempt policy; provider fallback already exhausted)",
					zap.String("channel", msg.Channel),
					zap.String("tenant_id", msg.TenantID),
					zap.String("request_id", msg.RequestID),
					zap.Strings("to", msg.To),
					zap.Uint64("attempts", attempt),
					zap.Error(deliverErr),
				)
				_ = m.Ack() // dead-letter: do not redeliver and hammer a blocked/rate-limited provider
			} else {
				// NAck triggers redelivery after AckWait (30s) — only reachable if maxDeliveryAttempts is raised >1
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
	}

	subscribeWithRetry(ctx, js, logg, "notifications worker", false, func() (*nats.Subscription, error) {
		return js.Subscribe(subject, msgHandler, nats.Durable(durable), nats.ManualAck(), nats.AckWait(30*time.Second), nats.MaxDeliver(maxDeliveryAttempts))
	})

	// Start fleet lifecycle event consumer (logistics-service → email notifications)
	startFleetConsumer(ctx, nc, js, cfg, tr, logg)

	// Start inventory stock event consumer (inventory-service → low stock alerts)
	startInventoryConsumer(ctx, nc, js, cfg, tr, logg)

	// Start order status event consumer (ordering-service → customer notifications)
	startOrderConsumer(ctx, nc, js, cfg, tr, logg)

	// Start subscription lifecycle event consumer (subscriptions-service → tenant admin notifications)
	startSubscriptionConsumer(ctx, nc, js, cfg, tr, logg)

	// Start auth notification consumer (auth-service → welcome emails, plain NATS)
	startAuthNotificationConsumer(ctx, nc, cfg, tr, logg)

	// Start treasury event consumer (treasury-service → payment/invoice notifications + credit top-ups)
	startTreasuryConsumer(ctx, nc, js, cfg, tr, billingSvc, whatsappSubsSvc, logg)

	// Start delivery task event consumer (logistics-service → delivery status notifications)
	startDeliveryConsumer(ctx, nc, js, cfg, tr, logg)

	// Start POS event consumer (pos-service → order/payment notifications)
	startPosConsumer(ctx, nc, js, cfg, tr, logg)

	// Start scheduled plan expiry warning notifier
	if cfg.Services.SubscriptionsAPI != "" {
		subsCfg := serviceclient.DefaultConfig(cfg.Services.SubscriptionsAPI, "subscriptions-api", logg)
		subsClient := serviceclient.New(subsCfg)
		scheduledNotifier := NewScheduledNotifier(logg, subsClient, nc, cfg, tr)
		scheduledNotifier.Start(ctx)
	}

	// Start ticketing event consumer (ticketing-service → ticket notifications)
	startTicketingConsumer(ctx, nc, js, cfg, tr, logg)

	// Start projects event consumer (projects-service → milestone notifications)
	startProjectsConsumer(ctx, nc, js, cfg, tr, logg)

	// Ensure digitika JetStream stream exists (published by codevertex-website)
	if _, streamErr := js.StreamInfo("digitika"); streamErr != nil {
		if _, streamErr = js.AddStream(&nats.StreamConfig{
			Name:      "digitika",
			Subjects:  []string{"digitika.>"},
			Retention: nats.LimitsPolicy,
			Storage:   nats.FileStorage,
			MaxAge:    7 * 24 * time.Hour,
		}); streamErr != nil {
			logg.Warn("digitika stream ensure failed (non-fatal)", zap.Error(streamErr))
		}
	}

	// Start digitika event consumer (codevertex-website → enrollment/payment notifications)
	startDigitikaConsumer(ctx, nc, js, cfg, tr, logg)

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
			if t.Website != "" {
				data["brand_website"] = t.Website
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
			// Resolve app URL for this message with cascading priority:
			// 1. Caller explicitly set app_url (service knows its own URL — strongest signal)
			// 2. service_id in message metadata → per-tenant ServiceURLs[service_id]
			// 3. tenant-level AppURL (default, for single-service tenants or SSO-only auth events)
			if _, callerSet := data["app_url"]; !callerSet {
				if svcID, _ := msg.Metadata["service_id"].(string); svcID != "" {
					if u, ok := t.ServiceURLs[svcID]; ok && u != "" {
						data["app_url"] = u
					}
				}
				if _, still := data["app_url"]; !still && t.AppURL != "" {
					data["app_url"] = t.AppURL
				}
			}
			// Auto-build getting_started_link if the caller didn't supply one.
			if _, ok := data["getting_started_link"]; !ok {
				appURL, _ := data["app_url"].(string)
				if appURL != "" {
					data["getting_started_link"] = appURL + "/" + t.Slug
				}
			}
		} else {
			logg.Warn("failed to load tenant branding", zap.String("tenant_id", msg.TenantID), zap.Error(err))
		}

		if _, ok := data["brand_name"]; !ok || data["brand_name"] == "" {
			data["brand_name"] = "Notifications"
		}

		// Auto-set sent_at if not provided by the caller
		if _, ok := data["sent_at"]; !ok {
			data["sent_at"] = time.Now().Format(time.RFC1123)
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
func deliver(ctx context.Context, cfg *config.Config, pm *providers.Manager, billingSvc *billing.Service, tr *tenantResolver, msg *messaging.Message, rendered string, logg *zap.Logger) error {
	channel := strings.ToLower(msg.Channel)
	preferred := ""
	if p, ok := msg.Metadata["provider"].(string); ok {
		preferred = p
	}

	// msg.TenantID may be a slug (e.g. "kura") rather than a UUID.
	// provider_settings rows are keyed by the notifications-api internal UUID, so
	// resolve slug → UUID before any provider or billing lookup.
	resolvedTenantID := msg.TenantID
	tenantID, parseErr := uuid.Parse(msg.TenantID)
	if parseErr != nil {
		if t, err := tr.resolve(ctx, msg.TenantID); err == nil {
			resolvedTenantID = t.ID.String()
			tenantID = t.ID
		} else {
			logg.Warn("tenant resolve failed for provider lookup, using raw tenant id",
				zap.String("tenant_id", msg.TenantID), zap.Error(err))
		}
	}

	// Determine which tenant ID to use for provider resolution based on sender scope.
	// Platform scope uses platform provider; tenant scope uses tenant's own provider.
	providerTenantID := resolvedTenantID
	if msg.EffectiveSenderScope() == messaging.SenderScopePlatform {
		providerTenantID = pm.PlatformID
	}

	switch channel {
	case "email":
		subject := "Notification"
		if s, ok := msg.Metadata["subject"].(string); ok && s != "" {
			subject = s
		}
		// Compose "from" with tenant brand name for display (e.g., "Urban Loft Cafe <info@...>")
		fromOverride := ""
		if brandName, ok := msg.Data["brand_name"].(string); ok && brandName != "" {
			fromOverride = brandName // Provider will wrap as "BrandName <configured-email>"
		}
		// Optional attachments (e.g. payslip PDFs the ERP sends on erp.email.requested).
		var atts []email.Attachment
		for _, a := range msg.Attachments {
			if len(a.Content) == 0 {
				continue
			}
			atts = append(atts, email.Attachment{Filename: a.Filename, ContentType: a.ContentType, Content: a.Content})
		}
		emailProv, _ := pm.GetEmailProvider(ctx, providerTenantID, preferred)
		err := emailProv.SendEmail(ctx, fromOverride, msg.To, msg.Cc, msg.Bcc, subject, rendered, "", atts)
		if err != nil {
			logg.Warn("tenant email delivery failed, trying platform fallback",
				zap.String("tenant_id", msg.TenantID),
				zap.String("provider", emailProv.Name()),
				zap.Error(err),
			)
			// Fallback to platform provider if tenant provider fails
			if providerTenantID != pm.PlatformID {
				platformProv, pErr := pm.GetEmailProvider(ctx, pm.PlatformID, "")
				if pErr == nil {
					if fbErr := platformProv.SendEmail(ctx, fromOverride, msg.To, msg.Cc, msg.Bcc, subject, rendered, "", atts); fbErr == nil {
						logg.Info("email sent via platform fallback",
							zap.String("provider", platformProv.Name()),
							zap.String("template", msg.TemplateID),
							zap.Strings("to", msg.To),
						)
						return nil
					}
				}
			}
			return err
		}
		logg.Info("email sent", zap.String("provider", emailProv.Name()), zap.String("template", msg.TemplateID), zap.Strings("to", msg.To))
		return nil

	case "sms":
		// Deduct credits based on segments and recipient count
		if err := billingSvc.DeductSMSCredits(ctx, tenantID, rendered, len(msg.To), "SMS Delivery"); err != nil {
			return fmt.Errorf("billing: %w", err)
		}

		smsProv, _ := pm.GetSMSProvider(ctx, providerTenantID, preferred)
		if err := smsProv.SendSMS(ctx, cfg.Providers.DefaultSMSSender, msg.To, rendered); err != nil {
			return err
		}
		logg.Info("sms sent", zap.String("provider", smsProv.Name()), zap.Strings("to", msg.To))
		return nil

	case "whatsapp":
		if err := billingSvc.DeductWhatsAppCredits(ctx, tenantID, len(msg.To), "WhatsApp Delivery"); err != nil {
			return fmt.Errorf("billing: %w", err)
		}

		waProv, err := pm.GetWhatsAppProvider(ctx, providerTenantID, preferred)
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
