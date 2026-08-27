package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"
	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/providers"
)

// TenantProviders handles tenant-level notification provider selection.
type TenantProviders struct {
	client      *ent.Client
	logger      *zap.Logger
	PlatformID  string
	keyProvider *encryption.KeyProvider
	manager     *providers.Manager
}

// NewTenantProviders creates a new TenantProviders handler.
// keyProvider resolves the provider-credential encryption key (DB-first, env fallback);
// when a key is available, secret values are encrypted at rest. manager is optional, used
// for the test-connection endpoint (mirrors PlatformProviders.TestProvider, tenant-scoped).
func NewTenantProviders(client *ent.Client, logger *zap.Logger, platformID string, keyProvider *encryption.KeyProvider, manager *providers.Manager) *TenantProviders {
	return &TenantProviders{client: client, logger: logger, PlatformID: platformID, keyProvider: keyProvider, manager: manager}
}

type availableProviderResponse struct {
	ProviderType string `json:"provider_type"`
	ProviderName string `json:"provider_name"`
	Environment  string `json:"environment"`
	IsActive     bool   `json:"is_active"`
}

type selectProviderRequest struct {
	ProviderType string `json:"provider_type"` // email, sms
	ProviderName string `json:"provider_name"` // smtp, sendgrid, twilio, etc.
	Environment  string `json:"environment"`   // sandbox, production
}

type brandingRedirectResponse struct {
	TenantID   string `json:"tenant_id"`
	TenantSlug string `json:"tenant_slug"`
	Message    string `json:"message"`
	AuthAPIURL string `json:"auth_api_url"`
}

// ListAvailable lists platform providers available for tenant selection.
func (h *TenantProviders) ListAvailable(w http.ResponseWriter, r *http.Request) {
	settings, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.Or(
				providersetting.TenantID(h.PlatformID),
				providersetting.TenantID("platform"),
			),
			providersetting.IsPlatform(true),
			providersetting.KeyEQ("_config"),
		).
		All(r.Context())
	if err != nil {
		h.logger.Error("failed to list available providers", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}

	result := make([]availableProviderResponse, 0, len(settings))
	for _, s := range settings {
		result = append(result, availableProviderResponse{
			ProviderType: s.ProviderType,
			ProviderName: s.ProviderName,
			Environment:  s.Environment,
			IsActive:     s.IsActive,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]any{"providers": result})
}

// SelectProvider sets the tenant's preferred provider for a channel.
func (h *TenantProviders) SelectProvider(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := httpware.GetTenantID(ctx)

	// Platform owners can override via query param, or fall back to their own tenant
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			tenantID = q
		} else if tenantID == "" {
			tenantID = h.PlatformID
		}
	}

	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}

	var req selectProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.ProviderType == "" || req.ProviderName == "" {
		jsonError(w, http.StatusBadRequest, "provider_type and provider_name are required")
		return
	}

	if req.Environment == "" {
		req.Environment = "production"
	}

	ctx = r.Context()

	// Verify platform provider exists
	count, _ := h.client.ProviderSetting.Query().
		Where(
			providersetting.Or(
				providersetting.TenantID(h.PlatformID),
				providersetting.TenantID("platform"),
			),
			providersetting.IsPlatform(true),
			providersetting.ProviderType(req.ProviderType),
			providersetting.ProviderName(req.ProviderName),
			providersetting.KeyEQ("_config"),
		).
		Count(ctx)
	if count == 0 {
		jsonError(w, http.StatusBadRequest, "provider not available on this platform")
		return
	}

	// Remove previous selection for this channel
	_, _ = h.client.ProviderSetting.Delete().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.ProviderType(req.ProviderType),
			providersetting.EnvironmentEQ(req.Environment),
			providersetting.KeyEQ("_preferred"),
		).
		Exec(ctx)

	// Create preferred provider selection
	_, err := h.client.ProviderSetting.Create().
		SetTenantID(tenantID).
		SetChannel(req.ProviderType).
		SetProvider(req.ProviderName).
		SetProviderType(req.ProviderType).
		SetProviderName(req.ProviderName).
		SetEnvironment(req.Environment).
		SetKey("_preferred").
		SetValue(req.ProviderName).
		SetIsPlatform(false).
		SetIsActive(true).
		SetStatus("active").
		Save(ctx)
	if err != nil {
		h.logger.Error("failed to select provider", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to select provider")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{
		"message":       "provider selected",
		"provider_type": req.ProviderType,
		"provider_name": req.ProviderName,
	})
}

// GetSelected returns the tenant's currently selected providers.
func (h *TenantProviders) GetSelected(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := httpware.GetTenantID(ctx)

	// Platform owners can override via query param
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			tenantID = q
		}
	}

	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}

	settings, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.KeyEQ("_preferred"),
		).
		All(r.Context())
	if err != nil {
		jsonError(w, http.StatusNotFound, "no selected providers")
		return
	}

	result := make([]map[string]string, 0, len(settings))
	for _, s := range settings {
		result = append(result, map[string]string{
			"provider_type": s.ProviderType,
			"provider_name": s.ProviderName,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]any{"selected": result})
}

// GetBranding returns a redirect to auth-api for tenant branding.
// Branding data (logo, colors, contact info) is managed exclusively by auth-api.
func (h *TenantProviders) GetBranding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := httpware.GetTenantID(ctx)

	// Platform owners can override via query param
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			tenantID = q
		}
	}

	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}

	t, err := h.client.Tenant.Query().
		Where(tenant.IDEQ(parseUUID(tenantID))).
		Only(ctx)
	if err != nil {
		jsonError(w, http.StatusNotFound, "tenant not found")
		return
	}

	resp := brandingRedirectResponse{
		TenantID:   t.ID.String(),
		TenantSlug: t.Slug,
		Message:    "Branding is managed by auth-api. Fetch branding from the auth_api_url below.",
		AuthAPIURL: "GET /api/v1/tenants/by-slug/" + t.Slug,
	}

	jsonResponse(w, http.StatusOK, resp)
}

// UpdateBranding returns 410 Gone — branding is managed at the SSO portal.
func (h *TenantProviders) UpdateBranding(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   "gone",
		"message": "Branding is managed at the SSO portal. Visit accounts.codevertexafrica.com/dashboard/settings to manage tenant branding.",
	})
}

// GetProviderSettings returns the tenant's settings for a specific provider (non-secret values only).
func (h *TenantProviders) GetProviderSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := httpware.GetTenantID(ctx)
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			tenantID = q
		} else if tenantID == "" {
			tenantID = h.PlatformID
		}
	}
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}

	providerType := r.URL.Query().Get("provider_type")
	providerName := r.URL.Query().Get("provider_name")
	if providerType == "" || providerName == "" {
		jsonError(w, http.StatusBadRequest, "provider_type and provider_name are required")
		return
	}

	settings, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.ProviderType(providerType),
			providersetting.ProviderName(providerName),
			providersetting.IsActive(true),
			providersetting.KeyNEQ("_config"),
			providersetting.KeyNEQ("_preferred"),
		).
		All(ctx)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to load settings")
		return
	}

	result := make(map[string]string)
	for _, s := range settings {
		if s.IsSecret || s.IsEncrypted {
			if s.Value != "" {
				result[s.Key] = "••••••••"
			} else {
				result[s.Key] = ""
			}
		} else {
			result[s.Key] = s.Value
		}
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"provider_type": providerType,
		"provider_name": providerName,
		"settings":      result,
	})
}

type saveTenantSettingsRequest struct {
	ProviderType string            `json:"provider_type"`
	ProviderName string            `json:"provider_name"`
	Settings     map[string]string `json:"settings"`
}

// SaveProviderSettings saves tenant-level provider settings.
func (h *TenantProviders) SaveProviderSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := httpware.GetTenantID(ctx)
	if httpware.IsPlatformOwner(ctx) {
		if q := r.URL.Query().Get("tenantId"); q != "" {
			tenantID = q
		} else if tenantID == "" {
			tenantID = h.PlatformID
		}
	}
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}

	var req saveTenantSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProviderType == "" || req.ProviderName == "" {
		jsonError(w, http.StatusBadRequest, "provider_type and provider_name are required")
		return
	}

	// Fetch existing settings before delete so we can preserve values not supplied in the request
	existingRows, _ := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.ProviderType(req.ProviderType),
			providersetting.ProviderName(req.ProviderName),
			providersetting.KeyNEQ("_config"),
			providersetting.KeyNEQ("_preferred"),
		).
		All(ctx)

	type existingVal struct {
		Value       string
		IsSecret    bool
		IsEncrypted bool
	}
	existing := make(map[string]existingVal, len(existingRows))
	for _, row := range existingRows {
		existing[row.Key] = existingVal{Value: row.Value, IsSecret: row.IsSecret, IsEncrypted: row.IsEncrypted}
	}

	_, _ = h.client.ProviderSetting.Delete().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.ProviderType(req.ProviderType),
			providersetting.ProviderName(req.ProviderName),
			providersetting.KeyNEQ("_config"),
			providersetting.KeyNEQ("_preferred"),
		).
		Exec(ctx)

	// Track which keys we've processed from the request
	processed := make(map[string]bool, len(req.Settings))

	for k, v := range req.Settings {
		processed[k] = true
		value := v
		isEncrypted := false
		isSecret := encryption.IsSecret(k)
		if old, ok := existing[k]; ok {
			isSecret = isSecret || old.IsSecret
		}

		// For empty or masked values, preserve existing DB value (including its encrypted state)
		if v == "" || v == "••••••••" {
			if old, ok := existing[k]; ok && old.Value != "" {
				value = old.Value
				isEncrypted = old.IsEncrypted
			} else {
				continue
			}
		} else if isSecret {
			// Encrypt new secret values at rest using the resolved primary key (if any).
			if enc, ok, encErr := h.keyProvider.Encrypt(ctx, v); encErr == nil && ok {
				value = enc
				isEncrypted = true
			}
		}

		_, err := h.client.ProviderSetting.Create().
			SetTenantID(tenantID).
			SetChannel(req.ProviderType).
			SetProvider(req.ProviderName).
			SetProviderType(req.ProviderType).
			SetProviderName(req.ProviderName).
			SetEnvironment("production").
			SetKey(k).
			SetValue(value).
			SetIsSecret(isSecret).
			SetIsEncrypted(isEncrypted).
			SetIsPlatform(false).
			SetIsActive(true).
			SetStatus("active").
			Save(ctx)
		if err != nil {
			h.logger.Error("failed to save provider setting", zap.String("key", k), zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "failed to save settings")
			return
		}
	}

	// Re-create existing settings that were not in the request (preserve untouched keys with their encrypted state)
	for k, old := range existing {
		if processed[k] || old.Value == "" {
			continue
		}
		_, err := h.client.ProviderSetting.Create().
			SetTenantID(tenantID).
			SetChannel(req.ProviderType).
			SetProvider(req.ProviderName).
			SetProviderType(req.ProviderType).
			SetProviderName(req.ProviderName).
			SetEnvironment("production").
			SetKey(k).
			SetValue(old.Value).
			SetIsSecret(old.IsSecret).
			SetIsEncrypted(old.IsEncrypted).
			SetIsPlatform(false).
			SetIsActive(true).
			SetStatus("active").
			Save(ctx)
		if err != nil {
			h.logger.Error("failed to re-create existing setting", zap.String("key", k), zap.Error(err))
		}
	}

	jsonResponse(w, http.StatusOK, map[string]string{"message": "settings saved"})
}

type testTenantProviderRequest struct {
	ProviderType string `json:"provider_type"`
	ProviderName string `json:"provider_name"`
	To           string `json:"to"` // Email or phone for test message
}

// TestProvider sends a live test message through the calling tenant's own configured provider
// (mirrors PlatformProviders.TestProvider, but resolved via LoadTenantProviderSettings's
// tenant-over-platform hierarchy so it tests exactly what a real send for this tenant would use).
func (h *TenantProviders) TestProvider(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		jsonError(w, http.StatusServiceUnavailable, "test connection is not available")
		return
	}
	ctx := r.Context()
	tenantID := httpware.GetTenantID(ctx)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	var req testTenantProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProviderType == "" || req.ProviderName == "" {
		jsonError(w, http.StatusBadRequest, "provider_type and provider_name are required")
		return
	}
	if req.ProviderType == "email" && req.To == "" {
		jsonError(w, http.StatusBadRequest, "to is required to test an email provider")
		return
	}

	info, err := h.manager.TestConnection(ctx, tenantID, req.ProviderType, req.ProviderName, req.To)
	if err != nil {
		h.logger.Warn("tenant provider test failed",
			zap.String("tenant_id", tenantID), zap.String("provider", req.ProviderName), zap.Error(err))
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   err.Error(),
			"message": "test connection failed",
		})
		return
	}

	message := "test message sent successfully"
	if info != nil {
		message = "connection verified"
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"success":       true,
		"provider_type": req.ProviderType,
		"provider_name": req.ProviderName,
		"message":       message,
		"info":          info,
	})
}

func parseUUID(s string) uuid.UUID {
	u, _ := uuid.Parse(s)
	return u
}

// RegisterTenantProviderRoutes registers tenant provider routes.
func (h *TenantProviders) RegisterTenantProviderRoutes(r chi.Router) {
	r.Route("/providers", func(prov chi.Router) {
		prov.Get("/available", h.ListAvailable)
		prov.Post("/select", h.SelectProvider)
		prov.Get("/selected", h.GetSelected)
		prov.Get("/settings", h.GetProviderSettings)
		prov.Post("/settings", h.SaveProviderSettings)
		prov.Post("/test", h.TestProvider)
	})
	r.Get("/branding", h.GetBranding)
	r.Put("/branding", h.UpdateBranding)
}
