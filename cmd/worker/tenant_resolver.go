package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	cache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/modules/tenant"
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
	// AppURL is the per-tenant primary app base URL (e.g. "https://kuraweigh.kura.go.ke").
	// Distinct from Website (corporate site). Stored in auth-api tenant metadata["app_url"].
	// Used as the default for getting_started_link and other deep links in email templates.
	AppURL         string
	// ServiceURLs maps service names to their per-tenant deployed frontend URLs.
	// e.g. {"truload": "https://kuraweigh.kura.go.ke", "logistics": "https://logistics.kura.go.ke"}
	// Stored in auth-api tenant metadata["service_urls"]. Takes priority over AppURL for
	// service-specific email links (order links, rider dashboard, etc.).
	ServiceURLs    map[string]string
	LogoURL        string
	PrimaryColor   string
	SecondaryColor string
}

// ServiceURL returns the per-tenant URL for the given service name.
// Falls back to the global env-var URL (envVar) and then to fallback.
// Strips trailing slash from all candidates.
func (t *tenantInfo) ServiceURL(service, envVar, fallback string) string {
	if t != nil {
		if u, ok := t.ServiceURLs[service]; ok && u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	if envVar != "" {
		if u := serviceURL(envVar, ""); u != "" {
			return u
		}
	}
	return strings.TrimRight(fallback, "/")
}

// tenantResolver resolves tenant details from local DB + Redis-cached auth-api data.
type tenantResolver struct {
	client  *ent.Client
	cache   *cache.Aside
	authURL string
	syncer  *tenant.Syncer
}

func newTenantResolver(client *ent.Client, c *cache.Aside, authURL string) *tenantResolver {
	return &tenantResolver{client: client, cache: c, authURL: authURL, syncer: tenant.NewSyncer(client, authURL)}
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
// Accepts either a UUID string or a slug; if tenantID is not a valid UUID, resolves by slug.
func (r *tenantResolver) resolve(ctx context.Context, tenantID string) (*tenantInfo, error) {
	id, err := uuid.Parse(tenantID)
	if err != nil {
		// tenantID is a slug (e.g. "kura") — fall back to slug-based resolution
		return r.resolveBySlug(ctx, tenantID)
	}

	t, err := r.client.Tenant.Query().
		Where(enttenant.IDEQ(id)).
		Only(ctx)
	if err != nil {
		if !ent.IsNotFound(err) {
			return nil, fmt.Errorf("tenant_resolver: tenant %s not found: %w", tenantID, err)
		}
		// Local tenant projection hasn't caught up yet (e.g. an event for a tenant created
		// moments earlier). Fall back to a live fetch-and-persist from auth-api rather than
		// failing the whole event — mirrors tenant.Syncer.SyncTenant's by-slug fallback.
		t, err = r.syncer.SyncTenantByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("tenant_resolver: tenant %s not found: %w", tenantID, err)
		}
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
	// AppURL and ServiceURLs come from auth-api tenant metadata — per-tenant deployed app domains.
	if details.Metadata != nil {
		if v, ok := details.Metadata["app_url"].(string); ok && v != "" {
			info.AppURL = normalizeWebsite(v)
		}
		// service_urls: {"truload": "https://...", "logistics": "https://...", ...}
		if raw, ok := details.Metadata["service_urls"]; ok {
			if m, ok := raw.(map[string]interface{}); ok {
				urls := make(map[string]string, len(m))
				for svc, val := range m {
					if u, ok := val.(string); ok && u != "" {
						urls[svc] = normalizeWebsite(u)
					}
				}
				if len(urls) > 0 {
					info.ServiceURLs = urls
				}
			}
		}
	}
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
