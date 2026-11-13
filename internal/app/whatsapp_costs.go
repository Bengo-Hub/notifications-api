package app

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/fxrate"
	handlers "github.com/bengobox/notifications-api/internal/http/handlers"
	"github.com/bengobox/notifications-api/internal/modules/credits"
	"github.com/bengobox/notifications-api/internal/modules/wacost"
	"github.com/bengobox/notifications-api/internal/providers"
)

// costAccounts adapts the provider manager to the report's account source.
type costAccounts struct{ m *providers.Manager }

func (c costAccounts) WhatsAppCostAccounts(ctx context.Context) ([]wacost.Account, error) {
	in, err := c.m.WhatsAppCostAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]wacost.Account, 0, len(in))
	for _, a := range in {
		out = append(out, wacost.Account{
			TenantID: a.TenantID, IsPlatform: a.IsPlatform, WABAID: a.WABAID, PhoneNumberID: a.PhoneNumberID,
			AccessToken: a.AccessToken, TokenSource: a.TokenSource, APIVersion: a.APIVersion,
		})
	}
	return out, nil
}

// tenantSlugs names tenants from the local tenant table.
func tenantSlugs(client *ent.Client) wacost.SlugResolver {
	return func(ctx context.Context, ids []string) map[string]string {
		uids := make([]uuid.UUID, 0, len(ids))
		for _, id := range ids {
			if u, err := uuid.Parse(id); err == nil {
				uids = append(uids, u)
			}
		}
		out := map[string]string{}
		if client == nil || len(uids) == 0 {
			return out
		}
		rows, err := client.Tenant.Query().Where(enttenant.IDIn(uids...)).All(ctx)
		if err != nil {
			return out
		}
		for _, t := range rows {
			out[t.ID.String()] = t.Slug
		}
		return out
	}
}

// whatsappCostsHandler builds the platform WhatsApp cost report handler.
// Meta's cost is converted to KES with treasury's rate (internal/fxrate, cached an hour).
func whatsappCostsHandler(m *providers.Manager, usage *credits.Client, client *ent.Client, rdb *redis.Client, rates *fxrate.Source, log *zap.Logger) *handlers.WhatsAppCostsHandler {
	var cache wacost.Cache
	if rdb != nil {
		cache = wacost.NewRedisCache(rdb)
	}
	graph := &wacost.Graph{BaseURL: wacost.DefaultGraphBaseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
	svc := wacost.NewService(costAccounts{m: m}, usage, graph, cache, tenantSlugs(client), log).WithRates(rates)
	return handlers.NewWhatsAppCostsHandler(svc, log)
}
