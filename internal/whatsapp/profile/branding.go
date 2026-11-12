package profile

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
)

// AuthAPIBranding reads a tenant's branding the way the worker's email templates do: the local
// tenant projection gives the slug, and auth-api's tenant record (cached in Redis through the
// shared cache library, fetched on a miss) gives the name, logo, email, website and metadata.
// aside may be nil (no Redis): every call then fetches from auth-api.
func AuthAPIBranding(client *ent.Client, aside *cache.Aside, authURL string) BrandingFunc {
	return func(ctx context.Context, tenantID string) (Branding, error) {
		if client == nil || strings.TrimSpace(authURL) == "" {
			return Branding{}, errors.New("auth-api is not configured")
		}
		slug := strings.TrimSpace(tenantID)
		if id, err := uuid.Parse(slug); err == nil {
			t, err := client.Tenant.Query().Where(enttenant.IDEQ(id)).Only(ctx)
			if err != nil {
				return Branding{}, fmt.Errorf("tenant %s not found: %w", tenantID, err)
			}
			slug = t.Slug
		}
		if slug == "" {
			return Branding{}, errors.New("tenant has no slug")
		}
		details, err := cache.GetTenantDetails(ctx, aside, authURL, slug, cache.DefaultTenantTTL)
		if err != nil {
			return Branding{}, err
		}
		return brandingFromDetails(details), nil
	}
}

// brandingFromDetails maps auth-api's tenant record. The tagline is what auth-ui's branding page
// saves as metadata "tagline"; description and address are read from metadata when present
// (auth-api has no dedicated fields for them).
func brandingFromDetails(d cache.TenantDetails) Branding {
	meta := func(k string) string {
		v, _ := d.Metadata[k].(string)
		return strings.TrimSpace(v)
	}
	return Branding{
		Name:        strings.TrimSpace(d.Name),
		About:       meta("tagline"),
		Description: meta("description"),
		Address:     meta("address"),
		Email:       strings.TrimSpace(d.ContactEmail),
		Website:     strings.TrimSpace(d.Website),
		LogoURL:     strings.TrimSpace(d.LogoURL),
	}
}
