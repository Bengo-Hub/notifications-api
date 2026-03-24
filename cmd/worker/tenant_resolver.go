package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
)

// tenantInfo holds the subset of tenant data needed by event consumers.
// Branding fields (ContactEmail, Phone, Website, Logo, Colors) come from
// Redis-cached auth-api data, not from the local DB.
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
	client *ent.Client
	rdb    *redis.Client
}

func newTenantResolver(client *ent.Client, rdb *redis.Client) *tenantResolver {
	return &tenantResolver{client: client, rdb: rdb}
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

	// Enrich with branding from Redis-cached auth-api data
	r.enrichFromCache(ctx, info)

	return info, nil
}

// resolveWithBranding looks up tenant by ID and returns full branding info from cached auth-api data.
func (r *tenantResolver) resolveWithBranding(ctx context.Context, tenantID string) (*tenantInfo, error) {
	// Same as resolve — all branding now comes from cache
	return r.resolve(ctx, tenantID)
}

// enrichFromCache populates branding fields from Redis-cached auth-api tenant data.
// Cache key: "tenant:{slug}" — set by auth-api with JWT TTL.
func (r *tenantResolver) enrichFromCache(ctx context.Context, info *tenantInfo) {
	if r.rdb == nil || info.Slug == "" {
		return
	}

	key := "tenant:" + info.Slug
	data, err := r.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return // cache miss is not an error
	}

	var cached struct {
		ContactEmail string         `json:"contact_email"`
		ContactPhone string         `json:"contact_phone"`
		Website      string         `json:"website"`
		LogoURL      string         `json:"logo_url"`
		BrandColors  map[string]any `json:"brand_colors"`
	}
	if err := json.Unmarshal(data, &cached); err != nil {
		return
	}

	info.ContactEmail = cached.ContactEmail
	info.ContactPhone = cached.ContactPhone
	info.Website = normalizeWebsite(cached.Website)
	info.LogoURL = cached.LogoURL
	if cached.BrandColors != nil {
		if v, ok := cached.BrandColors["primary"].(string); ok {
			info.PrimaryColor = v
		}
		if v, ok := cached.BrandColors["secondary"].(string); ok {
			info.SecondaryColor = v
		}
	}
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
