package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
)

// tenantInfo holds the subset of tenant data needed by event consumers.
type tenantInfo struct {
	ID             uuid.UUID
	Name           string
	ContactEmail   string
	ContactPhone   string
	Website        string
	LogoURL        string
	PrimaryColor   string
	SecondaryColor string
}

// tenantResolver resolves tenant details from the local DB.
type tenantResolver struct {
	client *ent.Client
}

func newTenantResolver(client *ent.Client) *tenantResolver {
	return &tenantResolver{client: client}
}

// resolve looks up tenant by ID string and returns contact/website info.
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

	return &tenantInfo{
		ID:           t.ID,
		Name:         t.Name,
		ContactEmail: t.ContactEmail,
		Website:      normalizeWebsite(t.Website),
	}, nil
}

// resolveWithBranding looks up tenant by ID and returns full branding info from the DB.
func (r *tenantResolver) resolveWithBranding(ctx context.Context, tenantID string) (*tenantInfo, error) {
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
		ID:           t.ID,
		Name:         t.Name,
		ContactEmail: t.ContactEmail,
		ContactPhone: t.ContactPhone,
		Website:      normalizeWebsite(t.Website),
		LogoURL:      t.LogoURL,
	}

	if t.BrandColors != nil {
		if v, ok := t.BrandColors["primary"].(string); ok {
			info.PrimaryColor = v
		}
		if v, ok := t.BrandColors["secondary"].(string); ok {
			info.SecondaryColor = v
		}
	}

	return info, nil
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
