package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
)

// Syncer handles dynamic syncing of tenant data from auth-api using Ent ORM.
type Syncer struct {
	client  *ent.Client
	authURL string
}

// NewSyncer creates a new TenantSyncer.
func NewSyncer(client *ent.Client, authURL string) *Syncer {
	return &Syncer{
		client:  client,
		authURL: authURL,
	}
}

// authAPITenantResponse is the minimal tenant JSON response from GET /api/v1/tenants/by-slug/{slug}.
// Only fields that this service persists locally are included.
type authAPITenantResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	Status  string `json:"status"`
	UseCase string `json:"use_case"`
}

// SyncTenant fetches the tenant record from auth-api and persists the minimal
// reference in the local PG DB using Ent.
func (s *Syncer) SyncTenant(ctx context.Context, slug string) (uuid.UUID, error) {
	// Fast path: check if tenant exists locally
	existing, err := s.client.Tenant.Query().Where(enttenant.SlugEQ(slug)).Only(ctx)
	if err == nil && existing != nil {
		return existing.ID, nil
	}

	endpoint := strings.TrimRight(s.authURL, "/") + "/api/v1/tenants/by-slug/" + slug

	log.Printf("  [tenant-sync] dynamically fetching %s from %s", slug, endpoint)
	resp, err := http.Get(endpoint) //nolint:noctx
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: tenant %q not found (404)", slug)
	}
	if resp.StatusCode != http.StatusOK {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: auth-api HTTP %d for %q", resp.StatusCode, slug)
	}

	var remote authAPITenantResponse
	if err := json.NewDecoder(resp.Body).Decode(&remote); err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: decode response: %w", err)
	}
	realID, err := uuid.Parse(remote.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: invalid UUID %q: %w", remote.ID, err)
	}

	now := time.Now()

	// Use Ent Upsert
	err = s.client.Tenant.Create().
		SetID(realID).
		SetName(remote.Name).
		SetSlug(remote.Slug).
		SetStatus(remote.Status).
		SetNillableUseCase(nillableStr(remote.UseCase)).
		SetSyncStatus("synced").
		SetLastSyncAt(now).
		OnConflictColumns("id").
		UpdateNewValues().
		Exec(ctx)

	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant.Syncer: upsert failed: %w", err)
	}

	log.Printf("  [tenant-sync] dynamically synced %s (UUID %s) into notifications-api DB", slug, realID)
	return realID, nil
}

// SyncTenantByID fetches the tenant record from auth-api by UUID and persists the minimal
// reference in the local PG DB using Ent. Used as a fallback when an event consumer only
// carries a tenant_id (no slug) and the local projection has not caught up yet — e.g. a
// subscription/order/inventory event arriving for a tenant created moments earlier, before
// the auth.tenant.created sync consumer (if any) has run. Mirrors SyncTenant's upsert.
func (s *Syncer) SyncTenantByID(ctx context.Context, id uuid.UUID) (*ent.Tenant, error) {
	// Fast path: check if tenant exists locally
	existing, err := s.client.Tenant.Query().Where(enttenant.IDEQ(id)).Only(ctx)
	if err == nil && existing != nil {
		return existing, nil
	}

	endpoint := strings.TrimRight(s.authURL, "/") + "/api/v1/tenants/by-id/" + id.String()

	log.Printf("  [tenant-sync] dynamically fetching %s from %s", id, endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("tenant.Syncer: build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tenant.Syncer: GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("tenant.Syncer: tenant %q not found (404)", id)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tenant.Syncer: auth-api HTTP %d for %q", resp.StatusCode, id)
	}

	var remote authAPITenantResponse
	if err := json.NewDecoder(resp.Body).Decode(&remote); err != nil {
		return nil, fmt.Errorf("tenant.Syncer: decode response: %w", err)
	}
	realID, err := uuid.Parse(remote.ID)
	if err != nil {
		return nil, fmt.Errorf("tenant.Syncer: invalid UUID %q: %w", remote.ID, err)
	}

	now := time.Now()

	err = s.client.Tenant.Create().
		SetID(realID).
		SetName(remote.Name).
		SetSlug(remote.Slug).
		SetStatus(remote.Status).
		SetNillableUseCase(nillableStr(remote.UseCase)).
		SetSyncStatus("synced").
		SetLastSyncAt(now).
		OnConflictColumns("id").
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant.Syncer: upsert failed: %w", err)
	}

	log.Printf("  [tenant-sync] dynamically synced %s (UUID %s) into notifications-api DB", remote.Slug, realID)
	return s.client.Tenant.Get(ctx, realID)
}

// nillableStr returns a *string if non-empty, nil otherwise.
func nillableStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
