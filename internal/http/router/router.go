package router

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	handlers "github.com/bengobox/notifications-api/internal/http/handlers"
	identityhandler "github.com/bengobox/notifications-api/internal/http/handlers/identity"
	"github.com/bengobox/notifications-api/internal/modules/identity"
	"github.com/bengobox/notifications-api/internal/modules/tenant"
	ratelimitmw "github.com/bengobox/notifications-api/internal/shared/middleware"
)

func New(log *zap.Logger, health *handlers.HealthHandler, notifications *handlers.NotificationHandler, templates *handlers.TemplateHandler, platformProviders *handlers.PlatformProviders, tenantProviders *handlers.TenantProviders, analytics *handlers.AnalyticsHandler, billing *handlers.BillingHandler, platformBilling *handlers.PlatformBilling, settings *handlers.SettingsHandler, rbacHandler *handlers.RBACHandler, authMeHandler *handlers.AuthMeHandler, deviceTokens *handlers.DeviceTokenHandler, apiKey string, authMiddleware *authclient.AuthMiddleware, authenticator *identityhandler.Authenticator, allowedOrigins []string, tenantSyncer *tenant.Syncer, rateLimiter *ratelimitmw.RateLimiter, serviceConfig *handlers.ServiceConfigHandler, whatsappSubs *handlers.WhatsAppSubscriptionHandler, backups *handlers.BackupHandler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(httpware.RequestID)
	r.Use(httpware.Logging(log))
	r.Use(httpware.Recover(log))
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Origin", "X-Request-ID", "X-Tenant-ID", "X-Tenant-Slug", "X-API-Key", "Idempotency-Key"},
		ExposedHeaders:   []string{"Link", "X-Request-ID", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	// Swagger UI
	r.Get("/v1/docs/*", handlers.SwaggerUI)

	// Redirect root path to Swagger documentation
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/docs/", http.StatusMovedPermanently)
	})

	r.Route("/api/v1", func(api chi.Router) {
		// Serve OpenAPI spec (public, no auth required)
		api.Get("/openapi.json", handlers.OpenAPIJSON)

		// Health endpoints (public)
		api.Get("/healthz", health.Liveness)
		api.Get("/readyz", health.Readiness)
		api.Get("/metrics", health.Metrics)

		// Templates — public platform-wide resource (no authentication required)
		api.Route("/templates", func(tmpl chi.Router) {
			tmpl.Get("/", templates.List)
			tmpl.Get("/*", templates.Get)
			tmpl.Post("/*", templates.TestSend) // handles /*/test
			tmpl.Put("/*", templates.Update)
		})

		// WhatsApp plans — public, no auth needed (pricing discovery)
		if whatsappSubs != nil {
			api.Get("/billing/whatsapp/plans", whatsappSubs.ListPlans)
		}

		// Protected routes - require authentication
		// NOTE: Notifications is a core service included in all subscription plans for free.
		// No RequireActiveSubscription — subscription enforcement is NOT applied.
		// Instead, email sending is rate-limited by subscription plan (max_emails_per_day).
		api.Group(func(protected chi.Router) {
			// Apply auth middleware if configured, otherwise allow API key
			if authMiddleware != nil {
				protected.Use(authMiddleware.RequireAuth)
			} else if apiKey != "" {
				protected.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Header.Get("X-API-Key") != apiKey {
							http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
							return
						}
						next.ServeHTTP(w, r)
					})
				})
			}

			// Layer 3: Identity — load/JIT-provision local user with roles & permissions
			if authenticator != nil {
				protected.Use(authenticator.RequireAuth)
			}

			// Platform admin routes (superuser-only)
			protected.Route("/platform", func(platform chi.Router) {
				if authenticator != nil {
					platform.Use(authenticator.RequireRoles(identity.RoleSuperAdmin))
				}
				platformProviders.RegisterPlatformProviderRoutes(platform)
				if serviceConfig != nil {
					serviceConfig.RegisterPlatformRoutes(platform)
					serviceConfig.RegisterTenantConfigRoutes(platform)
				}
				platform.Route("/billing", func(pb chi.Router) {
					if authenticator != nil {
						pb.Use(authenticator.RequirePermissions(identity.PermPlatformBilling))
					}
					pb.Get("/", platformBilling.GetSettings)
					pb.Post("/", platformBilling.UpdateSettings)
				})
			})

			// Analytics (platform or tenant-scoped)
			protected.Route("/analytics", func(analyticsRouter chi.Router) {
				if authenticator != nil {
					analyticsRouter.Use(authenticator.RequirePermissions(identity.PermAnalyticsRead))
				}
				analyticsRouter.Get("/delivery", analytics.Delivery)
				analyticsRouter.Get("/delivery/{tenantId}", analytics.Delivery)
				analyticsRouter.Get("/logs", analytics.Logs)
				analyticsRouter.Get("/logs/{tenantId}", analytics.Logs)
			})

			// Base group for tenant-scoped operations
			protected.Group(func(tenantRouter chi.Router) {
				tenantRouter.Use(httpware.TenantV2(httpware.TenantConfig{
					ClaimsExtractor: func(ctx context.Context) (tenantID, tenantSlug string, isPlatformOwner bool, ok bool) {
						claims, found := authclient.ClaimsFromContext(ctx)
						if !found {
							return "", "", false, false
						}
						// Slug-based platform owner check
						isPO := claims.GetTenantSlug() == "codevertex"
						return claims.TenantID, claims.GetTenantSlug(), isPO, true
					},
					URLParamFunc: chi.URLParam,
					Required:     false, // Make optional to allow platform owners to bypass
				}))

				// JIT tenant sync: ensure tenant exists in local DB when slug is in context
				if tenantSyncer != nil {
					tenantRouter.Use(func(next http.Handler) http.Handler {
						return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							slug := httpware.GetTenantSlug(r.Context())
							if slug != "" {
								if _, err := tenantSyncer.SyncTenant(r.Context(), slug); err != nil {
									log.Warn("tenant sync failed during request", zap.String("tenant_slug", slug), zap.Error(err))
								}
							}
							next.ServeHTTP(w, r)
						})
					})
				}

				// Service-level auth/me — returns user profile with local RBAC roles & permissions
				tenantRouter.Get("/auth/me", authMeHandler.GetMe)

				tenantRouter.Route("/notifications", func(notif chi.Router) {
					if authenticator != nil {
						notif.Use(authenticator.RequirePermissions(identity.PermNotificationsSend))
					}
					// Rate limit email sending by subscription plan (max_emails_per_day from JWT claims)
					if rateLimiter != nil {
						notif.Use(ratelimitmw.RequireRateLimit(rateLimiter, "max_emails_per_day"))
					}
					notif.Post("/messages", notifications.Enqueue)
				})

				// Tenant provider selection
				tenantProviders.RegisterTenantProviderRoutes(tenantRouter)

				// Billing routes
				tenantRouter.Route("/billing", func(b chi.Router) {
					b.Group(func(read chi.Router) {
						if authenticator != nil {
							read.Use(authenticator.RequirePermissions(identity.PermBillingRead))
						}
						read.Get("/balance", billing.GetBalance)
						read.Get("/transactions", billing.GetTransactions)
					})
					b.Group(func(write chi.Router) {
						if authenticator != nil {
							write.Use(authenticator.RequirePermissions(identity.PermBillingManage))
						}
						write.Post("/topup", billing.TopUp)
						write.Post("/initiate", billing.Initiate)
					})

					// WhatsApp subscription routes (under /billing/whatsapp)
					if whatsappSubs != nil {
						b.Route("/whatsapp", func(wa chi.Router) {
							// Public plan listing (no auth required inside the protected group context)
							wa.Get("/plans", whatsappSubs.ListPlans)
							wa.Group(func(read chi.Router) {
								if authenticator != nil {
									read.Use(authenticator.RequirePermissions(identity.PermBillingRead))
								}
								read.Get("/subscription", whatsappSubs.GetSubscription)
							})
							wa.Group(func(write chi.Router) {
								if authenticator != nil {
									write.Use(authenticator.RequirePermissions(identity.PermBillingManage))
								}
								write.Post("/subscribe", whatsappSubs.Subscribe)
								write.Post("/cancel", whatsappSubs.Cancel)
							})
						})
					}
				})

				// Settings routes
				tenantRouter.Route("/settings", func(s chi.Router) {
					if authenticator != nil {
						s.Use(authenticator.RequirePermissions(identity.PermSettingsRead))
					}
					s.Get("/security", settings.GetSecuritySettings)
				})

				// Tenant-scoped backups (this tenant's data only) — config/admin-gated.
				if backups != nil {
					tenantRouter.Group(func(bg chi.Router) {
						if authenticator != nil {
							bg.Use(authenticator.RequirePermissions(identity.PermSettingsManage))
						}
						backups.RegisterRoutes(bg)
					})
				}

				// Push notification device token management
				if deviceTokens != nil {
					tenantRouter.Route("/push/tokens", func(push chi.Router) {
						push.Post("/", deviceTokens.RegisterToken)
						push.Get("/", deviceTokens.ListTokens)
						push.Delete("/", deviceTokens.DeleteToken)
					})
				}

				// RBAC management routes
				if rbacHandler != nil {
					tenantRouter.Group(func(rbacRouter chi.Router) {
						if authenticator != nil {
							rbacRouter.Use(authenticator.RequirePermissions(identity.PermUsersManage))
						}
						rbacHandler.RegisterRoutes(rbacRouter)
					})
				}
			})
		})
	})

	return r
}
