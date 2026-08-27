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
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	sharedcache "github.com/Bengo-Hub/cache"
	eventslib "github.com/Bengo-Hub/shared-events"
	serviceclient "github.com/Bengo-Hub/shared-service-client"

	entdb "github.com/bengobox/notifications-api/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
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
	"github.com/bengobox/notifications-api/internal/modules/preferences"
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
				logg.Info(name + " subscription established")
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

	// NOTE: schema DDL is owned by the migrate binary (cmd/migrate, direct connection) run by the
	// entrypoint — NOT here. Running Schema.Create through the runtime (pgbouncer transaction-pooled)
	// connection is unsafe + redundant, so the worker no longer auto-migrates.

	// Initialize Treasury Client for top-up monitoring
	treasuryCfg := serviceclient.DefaultConfig(cfg.Services.TreasuryAPI, "treasury-api", logg)
	treasuryClient := serviceclient.New(treasuryCfg)

	billingSvc := billing.NewService(client, logg, treasuryClient, cfg.Security.APIKey)
	whatsappSubsSvc := billing.NewWhatsAppSubscriptionService(client, logg, treasuryClient, cfg.Security.APIKey)
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

	// Resolve the provider-credential decryption key DB-first (platform-owner-configurable
	// ServiceConfig encryption_key, tenant_id IS NULL) with env fallback.
	keyProvider := encryption.NewKeyProvider(client, cfg.Security.EncryptionKey)
	pm := providers.NewManager(dbPool, cfg.Postgres, cfg.Providers, keyProvider.Primary(ctx), cfg.App.Env, platformIDStr)
	emailGuardian := newEmailGuard(cfg.Providers.EmailMaxPerHour, cfg.Providers.EmailBurst, cfg.Providers.EmailValidateMX, logg)

	durable := "notifications-worker"
	// Redis for cached auth-api tenant data (branding, contact info)
	redisClient := cache.NewClient(cfg.Redis)

	// Shared cache-aside helper for tenant branding (auto-fetches from auth-api on miss)
	tenantCache := sharedcache.New(redisClient, logg)

	// Tenant resolver for event consumers and template branding
	tr := newTenantResolver(client, tenantCache, cfg.Services.AuthAPI)

	// Per-tenant notification gate: EVERY producer (domain-event consumers, HTTP/S2S
	// enqueue, ERP relay) converges on this worker, so enforcing here covers the whole
	// platform. Locked security templates always pass; essential types default ON;
	// optional types deliver only when the tenant enabled them in settings.
	prefGate := preferences.NewGate(client, redisClient, logg)

	msgHandler := func(m *nats.Msg) {
		var msg messaging.Message
		if err := json.Unmarshal(m.Data, &msg); err != nil {
			logg.Error("invalid message, dropping", zap.Error(err))
			_ = m.Ack() // unrecoverable — don't retry
			return
		}

		// Notification-preferences gate (before render/deliver). msg.TenantID may be a
		// slug — resolve to the UUID the ServiceConfig rows are keyed by.
		gateTenant := msg.TenantID
		if _, perr := uuid.Parse(gateTenant); perr != nil && gateTenant != "" {
			if t, rErr := tr.resolve(ctx, gateTenant); rErr == nil {
				gateTenant = t.ID.String()
			}
		}
		if !prefGate.Enabled(ctx, gateTenant, msg.TemplateID) {
			logg.Info("notification disabled by tenant preferences, dropping",
				zap.String("template", msg.TemplateID),
				zap.String("tenant_id", msg.TenantID),
				zap.String("channel", msg.Channel))
			_ = m.Ack()
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
		deliverErr := deliver(ctx, cfg, pm, emailGuardian, billingSvc, whatsappSubsSvc, tr, dbPool, &msg, rendered, logg)
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

	eventslib.SubscribeQueueWithRebind(logg, js, cfg.Events.StreamName, subject, durable, msgHandler, nats.Durable(durable), nats.ManualAck(), nats.AckWait(30*time.Second), nats.MaxDeliver(maxDeliveryAttempts))

	// Start fleet lifecycle event consumer (logistics-service → email notifications)
	startFleetConsumer(ctx, nc, js, cfg, tr, logg)

	// Start inventory stock event consumer (inventory-service → low stock alerts)
	startInventoryConsumer(ctx, nc, js, cfg, tr, logg)

	// Start library event consumer (library-service → overdue / hold-ready / fine / member alerts)
	startLibraryConsumer(ctx, nc, js, cfg, tr, logg)

	// Start order status event consumer (ordering-service → customer notifications)
	startOrderConsumer(ctx, nc, js, cfg, tr, logg)

	// Start subscription lifecycle event consumer (subscriptions-service → tenant admin notifications)
	startSubscriptionConsumer(ctx, nc, js, cfg, tr, logg)

	// Start auth notification consumer (auth-service → welcome emails, plain NATS)
	startAuthNotificationConsumer(ctx, nc, cfg, tr, logg)

	// Start reseller application decision consumer (auth-service → Certified Reseller
	// Program applicant approve/reject emails, plain NATS)
	startResellerApplicationNotificationConsumer(ctx, nc, cfg, logg)

	// Start treasury event consumer (treasury-service → payment/invoice notifications + credit top-ups)
	startTreasuryConsumer(ctx, nc, js, cfg, tr, billingSvc, whatsappSubsSvc, pm, dbPool, logg)

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
// verificationExemptTemplate lists the emails that must ALWAYS send even to an unverified
// account, because they are how the user verifies or recovers access. Gating these would
// lock an unverified user out permanently.
func verificationExemptTemplate(templateID string) bool {
	t := strings.ToLower(templateID)
	for _, p := range []string{"auth/welcome", "auth/password_reset", "auth/otp", "verify"} {
		if strings.Contains(t, p) {
			return true
		}
	}
	return false
}

// unverifiedUserRecipients returns the subset of `to` that map to a KNOWN local user whose
// email is not verified. Addresses with no user row (end customers, tenant contact_email,
// suppliers) are never returned — user-level gating must not touch them.
var (
	platformCCEmailMu      sync.Mutex
	platformCCEmailValue   string
	platformCCEmailExpires time.Time
)

// platformCCEmail returns the platform-wide BCC address (config key
// notifications.platform_cc_email, tenant_id=nil), cached in-process for 60s so the hot
// send path isn't a DB round-trip per email. Fail-open (empty string, no BCC applied) on
// any lookup error or when the setting hasn't been configured — this is a monitoring
// nice-to-have, never something that should block or retry a real send.
func platformCCEmail(ctx context.Context, db *pgxpool.Pool) string {
	platformCCEmailMu.Lock()
	defer platformCCEmailMu.Unlock()
	if time.Now().Before(platformCCEmailExpires) {
		return platformCCEmailValue
	}
	platformCCEmailExpires = time.Now().Add(60 * time.Second)
	platformCCEmailValue = ""
	if db == nil {
		return ""
	}
	var val string
	err := db.QueryRow(ctx,
		`SELECT config_value FROM service_configs WHERE config_key = 'notifications.platform_cc_email' AND tenant_id IS NULL`,
	).Scan(&val)
	if err != nil {
		return ""
	}
	platformCCEmailValue = strings.TrimSpace(val)
	return platformCCEmailValue
}

func unverifiedUserRecipients(ctx context.Context, db *pgxpool.Pool, to []string) map[string]bool {
	drop := map[string]bool{}
	if db == nil || len(to) == 0 {
		return drop
	}
	lowered := make([]string, len(to))
	for i, e := range to {
		lowered[i] = strings.ToLower(strings.TrimSpace(e))
	}
	rows, err := db.Query(ctx,
		`SELECT lower(email) FROM users WHERE lower(email) = ANY($1) AND email_verified = false`, lowered)
	if err != nil {
		return drop // fail-open: never block mail on a lookup error
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if rows.Scan(&e) == nil {
			drop[e] = true
		}
	}
	return drop
}

func deliver(ctx context.Context, cfg *config.Config, pm *providers.Manager, eg *emailGuard, billingSvc *billing.Service, whatsappSubsSvc *billing.WhatsAppSubscriptionService, tr *tenantResolver, dbPool *pgxpool.Pool, msg *messaging.Message, rendered string, logg *zap.Logger) error {
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
		fromOverride := msg.From // explicit full "name <email>"/bare-email override, e.g. from erp.email.requested's from_email
		if fromOverride == "" {
			if brandName, ok := msg.Data["brand_name"].(string); ok && brandName != "" {
				fromOverride = brandName // Provider will wrap as "BrandName <configured-email>"
			}
		}
		replyTo := msg.ReplyTo
		// Platform-scope transactional email with no explicit reply-to defaults to
		// info@ — decided explicitly (AskUserQuestion, 2026-08-18) rather than left
		// as a silent no-reply-to gap; tenant-scope messages are unaffected (each
		// tenant's own provider config/brand determines their reply behavior).
		if replyTo == "" && msg.EffectiveSenderScope() == messaging.SenderScopePlatform {
			replyTo = "info@codevertexafrica.com"
		}
		// Optional attachments (e.g. payslip PDFs the ERP sends on erp.email.requested).
		var atts []email.Attachment
		for _, a := range msg.Attachments {
			if len(a.Content) == 0 {
				continue
			}
			atts = append(atts, email.Attachment{Filename: a.Filename, ContentType: a.ContentType, Content: a.Content})
		}
		// Guard the shared provider account: skip invalid/unresolvable/suppressed recipients
		// (no guaranteed bounce, no noisy failure log) and pace the send under the provider's
		// rate limit so a backlog/burst never trips "mail rate exceeded".
		validTo, skipped := eg.ValidRecipients(msg.To)
		if len(skipped) > 0 {
			logg.Warn("skipped invalid email recipients",
				zap.Strings("skipped", skipped), zap.String("template", msg.TemplateID))
		}
		// Email-verification gate: drop recipients that ARE a known local user whose email
		// is unverified — that address is either a placeholder (undeliverable) or one the
		// user hasn't proven, and they are being pushed to verify at login. Addresses with
		// no user row (customers, tenant contacts, suppliers) pass untouched, and the
		// verify/welcome/reset templates are always allowed so users can still verify.
		if !verificationExemptTemplate(msg.TemplateID) {
			if drop := unverifiedUserRecipients(ctx, dbPool, validTo); len(drop) > 0 {
				kept := make([]string, 0, len(validTo))
				var gated []string
				for _, e := range validTo {
					if drop[strings.ToLower(strings.TrimSpace(e))] {
						gated = append(gated, e)
						continue
					}
					kept = append(kept, e)
				}
				if len(gated) > 0 {
					logg.Info("gated email to unverified account(s)",
						zap.Strings("gated", gated), zap.String("template", msg.TemplateID))
				}
				validTo = kept
			}
		}
		if len(validTo) == 0 {
			logg.Warn("no valid email recipients — skipping send",
				zap.Strings("to", msg.To), zap.String("template", msg.TemplateID))
			return nil // ack: nothing deliverable, do not retry
		}
		eg.WaitForSlot(ctx, 20*time.Second)

		// Circuit breaker: a tenant provider whose credentials recently failed auth is
		// skipped entirely — every attempt is a doomed AUTH against the SMTP host, and
		// bursts of failed logins trip the provider's abuse detection (Zoho 550 5.4.6).
		if providerTenantID != pm.PlatformID && eg.ProviderCoolingDown(providerTenantID) {
			providerTenantID = pm.PlatformID
			preferred = ""
		}

		// Real text/plain alternative, not "" — every email this worker sent
		// was HTML-only with no multipart/alternative part at all, a real
		// spam signal found in the 2026-08-19 deliverability audit.
		plainTextBody := email.HTMLToPlainText(rendered)

		// Platform oversight copy: every client/tenant-facing email also gets a silent BCC to
		// the platform's own notifications inbox (config key notifications.platform_cc_email,
		// tenant_id=nil — platform admin-settable via PUT /platform/config/{key}), so the team
		// keeps visibility on what actually goes out to tenants/customers, on top of the
		// existing per-template professional From/Reply-To addresses. BCC, not a visible CC —
		// a customer-facing invoice/receipt should not expose an internal monitoring address in
		// its headers. Never applied to Locked-class templates (OTP/password-reset/welcome):
		// those already exist specifically to carry a live credential/reset link, so copying
		// them into a second, more broadly-accessible inbox would be a real security regression,
		// not an oversight improvement.
		outboundBcc := msg.Bcc
		if !preferences.IsLocked(msg.TemplateID) {
			if ccEmail := platformCCEmail(ctx, dbPool); ccEmail != "" {
				outboundBcc = append(append([]string{}, msg.Bcc...), ccEmail)
			}
		}

		emailProv, _ := pm.GetEmailProvider(ctx, providerTenantID, preferred)
		err := emailProv.SendEmail(ctx, fromOverride, validTo, msg.Cc, outboundBcc, replyTo, subject, rendered, plainTextBody, atts)
		if err != nil {
			if providerTenantID != pm.PlatformID && isAuthFailureError(err) {
				eg.CoolProvider(providerTenantID, 30*time.Minute)
			}
			logg.Warn("tenant email delivery failed, trying platform fallback",
				zap.String("tenant_id", msg.TenantID),
				zap.String("provider", emailProv.Name()),
				zap.Error(err),
			)
			// Fallback to platform provider if tenant provider fails
			if providerTenantID != pm.PlatformID {
				platformProv, pErr := pm.GetEmailProvider(ctx, pm.PlatformID, "")
				if pErr == nil {
					if fbErr := platformProv.SendEmail(ctx, fromOverride, validTo, msg.Cc, msg.Bcc, replyTo, subject, rendered, plainTextBody, atts); fbErr == nil {
						logg.Info("email sent via platform fallback",
							zap.String("provider", platformProv.Name()),
							zap.String("template", msg.TemplateID),
							zap.Strings("to", validTo),
						)
						return nil
					}
				}
			}
			// Hard bounce (550 5.1.x) on a single recipient → suppress it so we stop sending
			// to a known-bad mailbox (protects sender reputation; not for 5.4.6 rate errors).
			if isPermanentRecipientError(err) && len(validTo) == 1 {
				eg.Suppress(validTo[0], 30*24*time.Hour)
			}
			return err
		}
		logg.Info("email sent", zap.String("provider", emailProv.Name()), zap.String("template", msg.TemplateID), zap.Strings("to", validTo))
		return nil

	case "sms":
		smsProv, _ := pm.GetSMSProvider(ctx, providerTenantID, preferred)

		// Platform-scope sends (OTPs, admin/ops alerts — see messaging.SenderScopePlatform) are
		// never billed to a tenant's own credit wallet, so gating them on a TenantCredit row
		// makes no sense — there's no tenant to charge. Instead, check the REAL provider account
		// balance directly (fail-open on a balance-check error: a monitoring hiccup must not
		// block an OTP or a security alert; the send itself would still fail cleanly if the real
		// balance is genuinely insufficient, now correctly surfaced instead of silently swallowed).
		if msg.EffectiveSenderScope() == messaging.SenderScopePlatform {
			if balProv, ok := smsProv.(interface {
				GetBalance(context.Context) (float64, error)
			}); ok {
				if bal, balErr := balProv.GetBalance(ctx); balErr == nil && bal <= 0 {
					logg.Warn("sms send skipped: real provider account balance is zero (platform-scope, not tenant-billed)",
						zap.String("template", msg.TemplateID))
					return nil
				}
			}
			if err := smsProv.SendSMS(ctx, cfg.Providers.DefaultSMSSender, msg.To, rendered); err != nil {
				return err
			}
			logg.Info("sms sent (platform-scope, billed to the real provider account, not a tenant wallet)",
				zap.String("provider", smsProv.Name()), zap.Strings("to", msg.To))
			return nil
		}

		// Tenant-scope sends (everything else — the overwhelming majority): every tenant must
		// have purchased SMS credit, billed in KES via PlatformBilling.CostPerSms, REGARDLESS of
		// whether their send ends up using their own configured provider or silently falls back
		// to the platform's shared credentials (the gate below is keyed purely on tenantID, before
		// provider resolution, so the fallback case is charged identically — never send for free).
		// FAIL-CLOSED: never send SMS unless we can positively confirm the tenant has credit. A
		// missing credit account returns balance 0 (-> skip); a balance-check error also skips (we
		// must not send uncharged). Skip+ack rather than error so the single-attempt worker
		// doesn't noisily dead-letter; the tenant must top up.
		balance, balErr := billingSvc.GetBalance(ctx, tenantID, "SMS")
		if balErr != nil {
			logg.Warn("sms send skipped: credit balance check failed (fail-closed)",
				zap.String("tenant_id", tenantID.String()),
				zap.String("template", msg.TemplateID),
				zap.Error(balErr),
			)
			return nil
		}
		if balance <= 0 {
			logg.Warn("sms send skipped: insufficient credits",
				zap.String("tenant_id", tenantID.String()),
				zap.String("template", msg.TemplateID),
				zap.Float64("balance", balance),
			)
			return nil // ack: nothing to retry until the tenant tops up
		}

		if err := smsProv.SendSMS(ctx, cfg.Providers.DefaultSMSSender, msg.To, rendered); err != nil {
			return err
		}
		// Deduct credits only after a successful send (segments × recipients).
		if err := billingSvc.DeductSMSCredits(ctx, tenantID, rendered, len(msg.To), "SMS Delivery"); err != nil {
			logg.Warn("sms sent but credit deduction failed", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		}
		logg.Info("sms sent", zap.String("provider", smsProv.Name()), zap.Strings("to", msg.To))
		return nil

	case "whatsapp":
		// The platform tenant (codevertex itself) is explicitly exempt from needing a purchased
		// WhatsApp subscription — every OTHER tenant must have one, active, to send at all. The
		// platform sends against its own real Meta WhatsApp Business Account directly; there's no
		// "subscription" to buy from itself, and no separate per-message credit charge either
		// (mirrors the SMS platform-scope precedent: real-provider-billed, not tenant-wallet-billed).
		isPlatformTenant := tenantID.String() == pm.PlatformID
		if !isPlatformTenant {
			// Pre-send subscription/quota gate (applies to BOTH HTTP-enqueued and event-sourced
			// sends). Skip+ack when the tenant has no active WhatsApp subscription or has
			// exhausted its quota. CheckQuota also increments the monthly message counter on
			// success. FAIL-CLOSED: never send WhatsApp without an active subscription/quota. If
			// the subscription service isn't wired, skip rather than send unmetered.
			if whatsappSubsSvc == nil {
				logg.Warn("whatsapp send skipped: subscription service unavailable (fail-closed)",
					zap.String("tenant_id", tenantID.String()), zap.String("template", msg.TemplateID))
				return nil
			}
			// The WhatsAppPlan subscription (checked above) is the ONE, centralized billing
			// mechanism for WhatsApp — a monthly fee for a bundled message quota, matching how
			// this platform's plan pricing is actually set (see docs/sprints/notifications-billing-
			// sprint.md). There used to ALSO be a per-message TenantCredit/PlatformBilling
			// deduction here (billingSvc.DeductWhatsAppCredits) — a second, independent charge a
			// tenant would need to separately fund on top of their subscription, which nothing
			// ever surfaced as a real product (no tenant has ever had a WHATSAPP-type TenantCredit
			// balance) and which the platform tenant's exemption above would have made
			// inconsistent to reason about. Removed rather than left redundant.
			if quotaErr := whatsappSubsSvc.CheckQuota(ctx, tenantID); quotaErr != nil {
				logg.Warn("whatsapp send skipped: no active subscription/quota",
					zap.String("tenant_id", tenantID.String()),
					zap.String("template", msg.TemplateID),
					zap.Error(quotaErr),
				)
				return nil // ack: nothing to retry until the tenant subscribes / quota resets
			}
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
