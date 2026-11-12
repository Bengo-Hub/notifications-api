package tenanttemplates

import (
	"context"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/deliverylog"
)

// UsageSource names the services a tenant uses (service tags, product codes or template id
// service segments); unknown names are ignored by Selection.
type UsageSource func(ctx context.Context, tenantID string) ([]string, error)

// ClaimsServices reads the active service tags and products from the caller's token, but only when
// the token belongs to tenantID: a platform owner acting for another tenant carries the platform
// tenant's own entitlements, which say nothing about that tenant.
func ClaimsServices(ctx context.Context, tenantID string) ([]string, error) {
	claims, ok := authclient.ClaimsFromContext(ctx)
	if !ok || claims == nil || claims.TenantID != tenantID {
		return nil, nil
	}
	out := make([]string, 0, len(claims.ActiveServiceTags)+len(claims.ActiveProducts))
	out = append(out, claims.ActiveServiceTags...)
	out = append(out, claims.ActiveProducts...)
	return out, nil
}

// DeliveryLogWindow is how far back DeliveryLogServices looks for a tenant's sends.
const DeliveryLogWindow = 180 * 24 * time.Hour

// DeliveryLogServices lists the services whose messages the tenant was sent within window, on any
// channel and with any outcome (a WhatsApp send skipped for want of a template still shows the
// tenant uses that service). Served by the (tenant_id, created_at) index.
func DeliveryLogServices(client *ent.Client, window time.Duration) UsageSource {
	return func(ctx context.Context, tenantID string) ([]string, error) {
		if client == nil {
			return nil, nil
		}
		ids, err := client.DeliveryLog.Query().
			Where(
				deliverylog.TenantID(tenantID),
				deliverylog.CreatedAtGTE(time.Now().Add(-window)),
			).
			Unique(true).
			Select(deliverylog.FieldTemplateID).
			Strings(ctx)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		out := []string{}
		for _, id := range ids {
			if svc := ServiceFromTemplateID(id); svc != "" && !seen[svc] {
				seen[svc] = true
				out = append(out, svc)
			}
		}
		return out, nil
	}
}
