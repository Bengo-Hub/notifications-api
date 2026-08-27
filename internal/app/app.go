package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"

	"database/sql"

	sharedcache "github.com/Bengo-Hub/cache"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/bengobox/notifications-api/internal/config"
	entdb "github.com/bengobox/notifications-api/internal/database"
	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/ent"
	handlers "github.com/bengobox/notifications-api/internal/http/handlers"
	identityhandler "github.com/bengobox/notifications-api/internal/http/handlers/identity"
	devauth "github.com/bengobox/notifications-api/internal/http/middleware"
	router "github.com/bengobox/notifications-api/internal/http/router"
	backupmod "github.com/bengobox/notifications-api/internal/modules/backup"
	"github.com/bengobox/notifications-api/internal/modules/billing"
	eventsmod "github.com/bengobox/notifications-api/internal/modules/events"
	"github.com/bengobox/notifications-api/internal/modules/identity"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
	"github.com/bengobox/notifications-api/internal/modules/rbac"
	templatesmod "github.com/bengobox/notifications-api/internal/modules/templates"
	"github.com/bengobox/notifications-api/internal/modules/tenant"
	"github.com/bengobox/notifications-api/internal/platform/cache"
	"github.com/bengobox/notifications-api/internal/platform/database"
	"github.com/bengobox/notifications-api/internal/platform/events"
	"github.com/bengobox/notifications-api/internal/platform/templates"
	"github.com/bengobox/notifications-api/internal/providers"
	sandboxmod "github.com/bengobox/notifications-api/internal/sandbox"
	"github.com/bengobox/notifications-api/internal/shared/logger"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type App struct {
	cfg                    *config.Config
	log                    *zap.Logger
	httpServer             *http.Server
	db                     *pgxpool.Pool
	entClient              *ent.Client
	cache                  *redis.Client
	events                 *nats.Conn
	templates              *templates.Loader
	outboxPublisher        *eventslib.Publisher
	treasuryClient         *serviceclient.Client
	crossServiceSubscriber *eventsmod.Subscriber
}

func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	log, err := logger.New(cfg.App.Env)
	if err != nil {
		return nil, fmt.Errorf("logger init: %w", err)
	}

	dbPool, err := database.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("postgres init: %w", err)
	}

	redisClient := cache.NewClient(cfg.Redis)

	natsConn, err := events.Connect(cfg.Events)
	if err != nil {
		log.Warn("event bus connection failed", zap.Error(err))
	}

	templateLoader := templates.New(cfg.Templates)

	// Initialize Ent client for provider management
	entClient, err := entdb.NewClient(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("ent client init: %w", err)
	}
	// Run auto-migrations
	if err := entdb.RunMigrations(ctx, entClient); err != nil {
		log.Warn("ent migration failed", zap.Error(err))
	}

	// Dedicated sql.DB for the backup module (raw tenant-row dump + advisory lock).
	backupSQLDB, err := sql.Open("pgx", cfg.Postgres.URL)
	if err != nil {
		return nil, fmt.Errorf("backup sql.DB init: %w", err)
	}
	backupSQLDB.SetMaxOpenConns(2)
	backupSQLDB.SetMaxIdleConns(1)

	// Initialize Treasury client
	treasuryCfg := serviceclient.DefaultConfig(cfg.Services.TreasuryAPI, "treasury-api", log.Named("treasury.client"))
	treasuryClient := serviceclient.New(treasuryCfg)

	// Sync platform owner tenant
	tenantSyncer := tenant.NewSyncer(entClient, cfg.Services.AuthAPI)
	platformID, err := tenantSyncer.SyncTenant(ctx, "codevertex")
	if err != nil {
		log.Warn("failed to sync platform owner, using fallback", zap.Error(err))
	}
	platformIDStr := platformID.String()

	healthHandler := handlers.NewHealthHandler(log, dbPool, redisClient, natsConn)
	billingService := billing.NewService(entClient, log, treasuryClient)
	whatsappSubsService := billing.NewWhatsAppSubscriptionService(entClient, log, treasuryClient, cfg.Security.APIKey)

	// Seed default WhatsApp plans on startup, then re-sync their provider-cost estimates against
	// the current centralized Meta-rate constant (SeedDefaultPlans only creates missing plans, it
	// never updates ones that already exist — this is the idempotent self-correction step so a
	// rate-constant fix takes effect on already-seeded plans without a manual one-off run).
	if seedErr := whatsappSubsService.SeedDefaultPlans(ctx); seedErr != nil {
		log.Warn("failed to seed default whatsapp plans", zap.Error(seedErr))
	}
	if recomputeErr := whatsappSubsService.RecomputeProviderCosts(ctx); recomputeErr != nil {
		log.Warn("failed to recompute whatsapp plan provider costs", zap.Error(recomputeErr))
	}

	whatsappSubsHandler := handlers.NewWhatsAppSubscriptionHandler(log, whatsappSubsService)
	notificationHandler := handlers.NewNotificationHandler(log, natsConn, redisClient, cfg.Events, entClient, cfg.Services.SubscriptionsURL, billingService, whatsappSubsService, platformIDStr)
	// Template repository backed by DB + Redis cache (2h TTL)
	var templateCache *sharedcache.Aside
	if redisClient != nil {
		templateCache = sharedcache.New(redisClient, log)
	}
	templateRepo := templatesmod.NewRepository(entClient, templateCache)
	templateHandler := handlers.NewTemplateHandler(templateLoader, templateRepo, notificationHandler)
	// Provider-credential encryption key: DB-first (platform-owner configurable via
	// ServiceConfig encryption_key, tenant_id IS NULL), env fallback
	// (SECURITY_ENCRYPTION_KEY). No dev fallback; absent => encryption disabled.
	keyProvider := encryption.NewKeyProvider(entClient, cfg.Security.EncryptionKey)

	// The provider Manager (used by the worker/test paths) consumes a bare key for
	// decryption at rest; seed it with the resolved primary key.
	providerManager := providers.NewManager(dbPool, cfg.Postgres, cfg.Providers, keyProvider.Primary(ctx), cfg.App.Env, platformIDStr)
	platformProviders := handlers.NewPlatformProviders(entClient, log, keyProvider, providerManager)
	tenantProviders := handlers.NewTenantProviders(entClient, log, platformIDStr, keyProvider)
	encryptionKeyHandler := handlers.NewEncryptionKeyHandler(entClient, log, keyProvider)
	analyticsHandler := handlers.NewAnalyticsHandler(entClient, log)

	deviceTokenHandler := handlers.NewDeviceTokenHandler(log, entClient)

	billingHandler := handlers.NewBillingHandler(log, billingService)
	platformBilling := handlers.NewPlatformBilling(entClient, log)
	settingsHandler := handlers.NewSettingsHandler(log, keyProvider)

	// Initialize identity module (RBAC)
	identityRepo := identity.NewEntRepository(entClient)
	identityService := identity.NewService(identityRepo, log, tenantSyncer)

	// Subscribe to auth-service events for user sync (if NATS available)
	if natsConn != nil {
		identityEvents := identity.NewEventHandler(identityService, log)
		if err := identityEvents.SubscribeToAuthEvents(natsConn); err != nil {
			log.Warn("failed to subscribe to auth events for identity sync", zap.Error(err))
		}
	}

	// Initialize auth-service JWT validator
	var authMiddleware *authclient.AuthMiddleware
	var authenticator *identityhandler.Authenticator
	if cfg.Security.RequireJWT {
		authConfig := authclient.DefaultConfig(
			cfg.Security.JWKSURL,
			cfg.Security.Issuer,
			cfg.Security.Audience,
		)
		// For local Docker development, skip TLS verification when connecting to auth-service
		// This allows mkcert certificates to work from inside containers
		var httpClient *http.Client
		if strings.Contains(cfg.Security.JWKSURL, "auth.codevertex.local") ||
			strings.Contains(cfg.Security.JWKSURL, "host.docker.internal") {
			tr := &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
			httpClient = &http.Client{
				Timeout:   10 * time.Second,
				Transport: tr,
			}
			authConfig.HTTPClient = httpClient
		}
		validator, err := authclient.NewValidator(authConfig)
		if err != nil {
			return nil, fmt.Errorf("auth validator init: %w", err)
		}

		// Create identity authenticator with RBAC middleware
		authenticator = identityhandler.NewAuthenticator(log, identityService, validator)

		// Add API key validator if enabled and database URL is provided
		if cfg.Security.EnableAPIKeyAuth && cfg.Security.APIKeyDBURL != "" {
			// Create HTTP client for API key validation
			apiKeyHTTPClient := &http.Client{Timeout: 10 * time.Second}
			// For local Docker development, skip TLS verification when connecting to auth-service
			if strings.Contains(cfg.Security.APIKeyDBURL, "auth.codevertex.local") ||
				strings.Contains(cfg.Security.APIKeyDBURL, "host.docker.internal") {
				tr := &http.Transport{
					TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
				}
				apiKeyHTTPClient = &http.Client{
					Timeout:   10 * time.Second,
					Transport: tr,
				}
			}
			apiKeyValidator := authclient.NewAPIKeyValidator(cfg.Security.APIKeyDBURL, apiKeyHTTPClient)
			authMiddleware = authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
		} else {
			authMiddleware = authclient.NewAuthMiddleware(validator)
		}
	}

	// Ensure JetStream stream for notifications events
	if natsConn != nil {
		if err := events.EnsureStream(ctx, natsConn, cfg.Events); err != nil {
			log.Warn("failed to ensure notifications JetStream stream (non-fatal)", zap.Error(err))
		} else {
			log.Info("notifications JetStream stream ensured", zap.String("stream", cfg.Events.StreamName))
		}
	}

	// Initialize outbox publisher
	var outboxPublisher *eventslib.Publisher
	if natsConn != nil && dbPool != nil {
		js, err := natsConn.JetStream()
		if err != nil {
			log.Warn("failed to get jetstream context, outbox publisher disabled", zap.Error(err))
		} else {
			// Get underlying sql.DB for outbox repository
			sqlDB, err := sql.Open("pgx", cfg.Postgres.URL)
			if err == nil {
				sqlDB.SetMaxOpenConns(2)
				sqlDB.SetMaxIdleConns(1)
				sqlDB.SetConnMaxLifetime(5 * time.Minute)
				sqlDB.SetConnMaxIdleTime(1 * time.Minute)
				outboxRepo := eventslib.NewSQLOutboxRepository(sqlDB)
				pubCfg := eventslib.DefaultPublisherConfig(js, outboxRepo, log)
				outboxPublisher = eventslib.NewPublisher(pubCfg)
				log.Info("outbox publisher initialized")
			} else {
				log.Warn("failed to create sql.DB for outbox, publisher disabled", zap.Error(err))
			}
		}
	}

	// Initialize RBAC module
	rbacRepo := rbac.NewEntRepository(entClient)
	rbacService := rbac.NewService(rbacRepo, log.Named("rbac"))
	rbacHandler := handlers.NewRBACHandler(log.Named("rbac.handler"), rbacService, rbacRepo)
	authMeHandler := handlers.NewAuthMeHandler(rbacService, log.Named("auth.me"), platformIDStr)

	// Initialize Redis-backed rate limiter for email sending by subscription plan
	var rateLimiter *ratelimit.Quota
	if redisClient != nil {
		rateLimiter = ratelimit.NewQuota(redisClient)
		log.Info("email rate limiter initialized (subscription plan limits via JWT claims)")
	}

	// Initialize service config handler for platform admin + tenant settings
	serviceConfigHandler := handlers.NewServiceConfigHandler(entClient, log)

	// Per-tenant notification-type toggles (feed the worker's dispatch gate; the gate
	// instance here is only used to invalidate the Redis cache on settings writes).
	prefGate := preferences.NewGate(entClient, redisClient, log)
	notificationPrefsHandler := handlers.NewPreferencesHandler(log, entClient, prefGate)

	// Pluggable backup destination (OneDrive/GDrive/S3/WebDAV/SFTP/SMB) — encrypted
	// at rest with a SECRET_KEY-derived AES-256 key. The handler owns the destination
	// Store; its Uploader is attached to the backup service so every PVC backup is
	// additionally mirrored best-effort.
	backupDestHandler := handlers.NewBackupDestinationHandler(entClient, log)

	// Tenant-scoped backups + daily 02:00 auto-backup scheduler + retention churn.
	backupSvc := backupmod.NewService(backupSQLDB, entClient, cfg.Backup.Dir, log).
		WithMirrorer(backupDestHandler.Uploader())
	backupHandler := handlers.NewBackupHandler(log, backupSvc, cfg.Backup.RetentionDays)
	backupmod.NewScheduler(backupSvc, backupmod.SchedulerConfig{
		Enabled:       cfg.Backup.ScheduleEnabled,
		Hour:          cfg.Backup.ScheduleHour,
		RetentionDays: cfg.Backup.RetentionDays,
	}, log).Start(ctx)

	developerKeyAuth := devauth.NewDeveloperKeyAuth(cfg.Services.AuthAPI, log)
	notificationHandler.SetSandboxStore(sandboxmod.New(redisClient))

	swaggerHandler, err := handlers.NewSwaggerHandler(authclient.NewAPIKeyValidator(cfg.Services.AuthAPI, nil))
	if err != nil {
		return nil, fmt.Errorf("build swagger handler: %w", err)
	}

	httpRouter := router.New(log, healthHandler, notificationHandler, templateHandler, platformProviders, tenantProviders, analyticsHandler, billingHandler, platformBilling, settingsHandler, rbacHandler, authMeHandler, deviceTokenHandler, cfg.Security.APIKey, authMiddleware, authenticator, cfg.HTTP.AllowedOrigins, tenantSyncer, rateLimiter, serviceConfigHandler, whatsappSubsHandler, backupHandler, encryptionKeyHandler, backupDestHandler, notificationPrefsHandler, developerKeyAuth, swaggerHandler)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           httpRouter,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	var crossServiceSubscriber *eventsmod.Subscriber
	if natsConn != nil {
		crossServiceSubscriber = eventsmod.New(natsConn, cfg.Events, log).
			WithAuthAPI(cfg.Services.AuthAPI)
	}

	return &App{
		cfg:                    cfg,
		log:                    log,
		httpServer:             httpServer,
		db:                     dbPool,
		entClient:              entClient,
		cache:                  redisClient,
		events:                 natsConn,
		templates:              templateLoader,
		outboxPublisher:        outboxPublisher,
		treasuryClient:         treasuryClient,
		crossServiceSubscriber: crossServiceSubscriber,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	// Start outbox publisher worker
	if a.outboxPublisher != nil {
		go func() {
			if err := a.outboxPublisher.Start(ctx); err != nil {
				a.log.Error("outbox publisher failed", zap.Error(err))
			}
		}()
		a.log.Info("outbox publisher started")
	}

	// Start cross-service event subscriber (inventory/KDS/payroll notifications)
	if a.crossServiceSubscriber != nil {
		go func() {
			if err := a.crossServiceSubscriber.Start(ctx); err != nil {
				a.log.Error("cross-service event subscriber stopped", zap.Error(err))
			}
		}()
		a.log.Info("cross-service NATS event subscriber started")
	}

	errCh := make(chan error, 1)
	if a.cfg.HTTP.TLSCertFile != "" && a.cfg.HTTP.TLSKeyFile != "" {
		a.log.Info("notifications service starting with HTTPS",
			zap.String("addr", a.httpServer.Addr),
			zap.String("cert", a.cfg.HTTP.TLSCertFile),
			zap.String("key", a.cfg.HTTP.TLSKeyFile),
		)
		go func() {
			errCh <- a.httpServer.ListenAndServeTLS(a.cfg.HTTP.TLSCertFile, a.cfg.HTTP.TLSKeyFile)
		}()
	} else {
		a.log.Info("notifications service starting with HTTP", zap.String("addr", a.httpServer.Addr))
		go func() {
			errCh <- a.httpServer.ListenAndServe()
		}()
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}

		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server error: %w", err)
	}
}

func (a *App) Close() {
	if a.events != nil {
		if err := a.events.Drain(); err != nil {
			a.log.Warn("nats drain failed", zap.Error(err))
		}
		a.events.Close()
	}

	if a.cache != nil {
		if err := a.cache.Close(); err != nil {
			a.log.Warn("redis close failed", zap.Error(err))
		}
	}

	if a.entClient != nil {
		if err := a.entClient.Close(); err != nil {
			a.log.Warn("ent client close failed", zap.Error(err))
		}
	}

	if a.db != nil {
		a.db.Close()
	}

	_ = a.log.Sync()
}
