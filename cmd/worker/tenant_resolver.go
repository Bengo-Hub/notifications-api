package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	cache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
)

// tenantInfo holds the subset of tenant data needed by event consumers.
// Branding fields (ContactEmail, Phone, Website, Logo, Colors) come from
// Redis-cached auth-api data via the shared cache library, not from the local DB.
type tenantInfo struct {
	ID             uuid.UUID
	Name           string
	Slug           string
	ContactEmail   string
	ContactPhone   string
	Website        string
	LogoURL        string
	PrimaryColor   string
	SecondaryColor string
}

// tenantResolver resolves tenant details from local DB + Redis-cached auth-api data.
type tenantResolver struct {
	client  *ent.Client
	cache   *cache.Aside
	authURL string
}

func newTenantResolver(client *ent.Client, c *cache.Aside, authURL string) *tenantResolver {
	return &tenantResolver{client: client, cache: c, authURL: authURL}
}

// resolveBySlug looks up a tenant by its slug and returns basic info + cached branding.
// Used by consumers that receive slug-based tenant identifiers (e.g. codevertex-website events).
func (r *tenantResolver) resolveBySlug(ctx context.Context, slug string) (*tenantInfo, error) {
	t, err := r.client.Tenant.Query().
		Where(enttenant.SlugEQ(slug)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant_resolver: tenant with slug %q not found: %w", slug, err)
	}

	info := &tenantInfo{
		ID:   t.ID,
		Name: t.Name,
		Slug: t.Slug,
	}

	r.enrichFromCache(ctx, info)

	return info, nil
}

// resolve looks up tenant by ID string and returns basic info + cached branding.
func (r *tenantResolver) resolve(ctx context.Context, tenantID string) (*tenantInfo, error) {
	id, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, fmt.Errorf("tenant_resolver: invalid tenant ID %q: %w", tenantID, err)
	}

	t, err := r.client.Tenant.Query().
		Where(enttenant.IDEQ(id)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant_resolver: tenant %s not found: %w", tenantID, err)
	}

	info := &tenantInfo{
		ID:   t.ID,
		Name: t.Name,
		Slug: t.Slug,
	}

	// Enrich with branding from Redis-cached auth-api data (cache-aside pattern)
	r.enrichFromCache(ctx, info)

	return info, nil
}

// resolveWithBranding looks up tenant by ID and returns full branding info from cached auth-api data.
func (r *tenantResolver) resolveWithBranding(ctx context.Context, tenantID string) (*tenantInfo, error) {
	// Same as resolve — all branding now comes from cache
	return r.resolve(ctx, tenantID)
}

// enrichFromCache populates branding fields from Redis-cached auth-api tenant data.
// Uses the shared cache library's cache-aside pattern: on cache miss, it automatically
// fetches from auth-api and populates Redis with TTL aligned to JWT lifetime.
func (r *tenantResolver) enrichFromCache(ctx context.Context, info *tenantInfo) {
	if r.cache == nil || info.Slug == "" || r.authURL == "" {
		return
	}

	details, err := cache.GetTenantDetails(ctx, r.cache, r.authURL, info.Slug, cache.DefaultTenantTTL)
	if err != nil {
		return // cache miss + fetch failure is not fatal
	}

	branding := cache.GetTenantBranding(details)
	info.ContactEmail = details.ContactEmail
	info.ContactPhone = details.ContactPhone
	info.Website = normalizeWebsite(details.Website)
	info.LogoURL = branding.LogoURL
	info.PrimaryColor = branding.PrimaryColor
	info.SecondaryColor = branding.SecondaryColor
}

// normalizeWebsite ensures the website has a scheme and no trailing slash.
func normalizeWebsite(w string) string {
	w = strings.TrimSpace(w)
	if w == "" {
		return ""
	}
	w = strings.TrimRight(w, "/")
	if !strings.HasPrefix(w, "http://") && !strings.HasPrefix(w, "https://") {
		w = "https://" + w
	}
	return w
}
