package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/notifications-app/internal/config"
	handlers "github.com/bengobox/notifications-app/internal/http/handlers"
	router "github.com/bengobox/notifications-app/internal/http/router"
	"github.com/bengobox/notifications-app/internal/platform/cache"
	"github.com/bengobox/notifications-app/internal/platform/database"
	"github.com/bengobox/notifications-app/internal/platform/events"
	"github.com/bengobox/notifications-app/internal/platform/templates"
	"github.com/bengobox/notifications-app/internal/shared/logger"
)

type App struct {
	cfg        *config.Config
	log        *zap.Logger
	httpServer *http.Server
	db         *pgxpool.Pool
	cache      *redis.Client
	events     *nats.Conn
	templates  *templates.Loader
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

	healthHandler := handlers.NewHealthHandler(log, dbPool, redisClient, natsConn)
	notificationHandler := handlers.NewNotificationHandler(log, natsConn, redisClient, cfg.Events)
	templateHandler := handlers.NewTemplateHandler(templateLoader)

	// Initialize auth-service JWT validator
	var authMiddleware *authclient.AuthMiddleware
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

		// Initialize API key validator if enabled
		if cfg.Security.EnableAPIKeyAuth {
			apiKeyValidator := authclient.NewAPIKeyValidator(cfg.Security.AuthServiceURL, httpClient)
			authMiddleware = authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
		} else {
			authMiddleware = authclient.NewAuthMiddleware(validator)
		}
	}

	ginRouter := router.New(log, healthHandler, notificationHandler, templateHandler, cfg.Security.APIKey, authMiddleware)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           ginRouter,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:        cfg,
		log:        log,
		httpServer: httpServer,
		db:         dbPool,
		cache:      redisClient,
		events:     natsConn,
		templates:  templateLoader,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	if a.cfg.App.Env == "development" {
		gin.SetMode(gin.DebugMode)
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

	if a.db != nil {
		a.db.Close()
	}

	_ = a.log.Sync()
}
