package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/credits"
	"github.com/bengobox/notifications-api/internal/modules/wacost"
)

type countingAccounts struct{ calls int64 }

func (c *countingAccounts) WhatsAppCostAccounts(context.Context) ([]wacost.Account, error) {
	atomic.AddInt64(&c.calls, 1)
	return nil, nil
}

type emptyUsage struct{}

func (emptyUsage) WhatsAppUsage(context.Context, string, string) (credits.WhatsAppUsage, error) {
	return credits.WhatsAppUsage{}, nil
}

type noMeta struct{}

func (noMeta) PhoneNumbers(context.Context, string, string, string) ([]wacost.PhoneNumber, error) {
	return nil, nil
}

func (noMeta) PricingAnalytics(context.Context, string, string, string, time.Time, time.Time) (wacost.Pricing, error) {
	return wacost.Pricing{}, nil
}

// The report shows every tenant's money: only the signed platform-owner claim may read it, even
// for a super admin of a tenant (the /platform group's role check is not enough on its own).
func TestWhatsAppCostsPlatformOwnerOnly(t *testing.T) {
	accounts := &countingAccounts{}
	h := NewWhatsAppCostsHandler(wacost.NewService(accounts, emptyUsage{}, noMeta{}, nil, nil, zap.NewNop()), zap.NewNop())
	r := chi.NewRouter()
	r.Route("/api/v1/platform", h.RegisterPlatformRoutes)

	call := func(c *authclient.Claims, query string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/platform/whatsapp/costs"+query, nil)
		if c != nil {
			req = req.WithContext(authclient.ContextWithClaims(req.Context(), c))
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := call(nil, ""); got != http.StatusForbidden {
		t.Fatalf("no claims answered %d, want 403", got)
	}
	if got := call(&authclient.Claims{TenantSlug: "codevertex", Roles: []string{"super_admin"}}, ""); got != http.StatusForbidden {
		t.Fatalf("a member without the platform-owner claim answered %d, want 403", got)
	}
	if atomic.LoadInt64(&accounts.calls) != 0 {
		t.Fatal("a refused caller must not trigger the report")
	}
	owner := &authclient.Claims{TenantSlug: "codevertex", IsPlatformOwner: true}
	if got := call(owner, "?from=2026-11-01&to=2026-11-12"); got != http.StatusOK {
		t.Fatalf("platform owner answered %d, want 200", got)
	}
	if got := call(owner, "?from=2026-11-12&to=2026-11-01"); got != http.StatusBadRequest {
		t.Fatalf("reversed range answered %d, want 400", got)
	}
}
