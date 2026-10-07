package router

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	handlers "github.com/bengobox/notifications-api/internal/http/handlers"
	devauth "github.com/bengobox/notifications-api/internal/http/middleware"
	"github.com/bengobox/notifications-api/internal/modules/tenant"
)

// TestRouterBuilds constructs the full route tree. chi panics at start-up when a middleware is
// added to a group after its first route; 2026-10-07 shipped exactly that (analytics mounted
// before the tenant-sync middleware) and the new pods crash-looped. Handlers are zero values:
// building the tree only takes their method values, it never calls them.
func TestRouterBuilds(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("router.New panicked: %v", r)
		}
	}()
	h := New(zap.NewNop(), new(handlers.HealthHandler), new(handlers.NotificationHandler), new(handlers.TemplateHandler),
		new(handlers.PlatformProviders), new(handlers.TenantProviders), new(handlers.AnalyticsHandler), new(handlers.BillingHandler),
		new(handlers.PlatformBilling), new(handlers.SettingsHandler), new(handlers.RBACHandler), new(handlers.AuthMeHandler),
		new(handlers.DeviceTokenHandler), "test-key", nil, nil, nil, new(tenant.Syncer), nil,
		new(handlers.ServiceConfigHandler), new(handlers.WhatsAppSubscriptionHandler), new(handlers.BackupHandler),
		new(handlers.EncryptionKeyHandler), new(handlers.BackupDestinationHandler), new(handlers.PreferencesHandler),
		new(devauth.DeveloperKeyAuth), new(handlers.SwaggerHandler), new(handlers.WebhookHandler),
		new(handlers.WhatsAppEmbeddedSignupHandler), new(handlers.WhatsAppTemplates), new(handlers.WhatsAppInboxHandler),
		new(handlers.AnnouncementHandler), new(handlers.BroadcastHandler))
	if h == nil {
		t.Fatal("router.New returned nil")
	}
	// An unknown path must be a plain 404, proving the tree serves requests.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/no-such-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown route: got %d", rec.Code)
	}
}
