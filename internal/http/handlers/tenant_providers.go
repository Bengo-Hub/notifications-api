package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	"github.com/bengobox/notifications-api/internal/ent/tenant"
	httpware "github.com/Bengo-Hub/httpware"
)

// TenantProviders handles tenant-level notification provider selection and branding.
type TenantProviders struct {
	client     *ent.Client
	logger     *zap.Logger
	PlatformID string
}

// NewTenantProviders creates a new TenantProviders handler.
func NewTenantProviders(client *ent.Client, logger *zap.Logger, platformID string) *TenantProviders {
	return &TenantProviders{client: client, logger: logger, PlatformID: platformID}
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

type brandingRequest struct {
	FromEmail      string `json:"from_email,omitempty"`
	FromName       string `json:"from_name,omitempty"`
	LogoURL        string `json:"logo_url,omitempty"`
	PrimaryColor   string `json:"primary_color,omitempty"`
	SecondaryColor string `json:"secondary_color,omitempty"`
}

type brandingResponse struct {
	TenantID       string `json:"tenant_id"`
	FromEmail      string `json:"from_email,omitempty"`
	FromName       string `json:"from_name,omitempty"`
	LogoURL        string `json:"logo_url,omitempty"`
	PrimaryColor   string `json:"primary_color,omitempty"`
	SecondaryColor string `json:"secondary_color,omitempty"`
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

// GetBranding returns the tenant's notification branding.
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
		Only(r.Context())
	if err != nil {
		jsonError(w, http.StatusNotFound, "tenant not found")
		return
	}

	resp := brandingResponse{
		TenantID:     t.ID.String(),
		LogoURL:      t.LogoURL,
	}

	if t.BrandColors != nil {
		if v, ok := t.BrandColors["primary"].(string); ok {
			resp.PrimaryColor = v
		}
		if v, ok := t.BrandColors["secondary"].(string); ok {
			resp.SecondaryColor = v
		}
	}

	// Get from_email and from_name from metadata
	if t.Metadata != nil {
		if v, ok := t.Metadata["from_email"].(string); ok {
			resp.FromEmail = v
		}
		if v, ok := t.Metadata["from_name"].(string); ok {
			resp.FromName = v
		}
	}

	jsonResponse(w, http.StatusOK, resp)
}

// UpdateBranding creates or updates the tenant's notification branding.
func (h *TenantProviders) UpdateBranding(w http.ResponseWriter, r *http.Request) {
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

	var req brandingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx = r.Context()

	t, err := h.client.Tenant.Query().
		Where(tenant.IDEQ(parseUUID(tenantID))).
		Only(ctx)
	if err != nil {
		jsonError(w, http.StatusNotFound, "tenant not found")
		return
	}

	update := t.Update()
	metadata := t.Metadata
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["from_email"] = req.FromEmail
	metadata["from_name"] = req.FromName
	update.SetMetadata(metadata)

	if req.LogoURL != "" {
		update.SetLogoURL(req.LogoURL)
	}

	colors := t.BrandColors
	if colors == nil {
		colors = make(map[string]any)
	}
	if req.PrimaryColor != "" {
		colors["primary"] = req.PrimaryColor
	}
	if req.SecondaryColor != "" {
		colors["secondary"] = req.SecondaryColor
	}
	update.SetBrandColors(colors)

	_, err = update.Save(ctx)
	if err != nil {
		h.logger.Error("failed to update branding", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to update branding")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"message": "branding updated"})
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

	_, _ = h.client.ProviderSetting.Delete().
		Where(
			providersetting.TenantID(tenantID),
			providersetting.ProviderType(req.ProviderType),
			providersetting.ProviderName(req.ProviderName),
			providersetting.KeyNEQ("_config"),
			providersetting.KeyNEQ("_preferred"),
		).
		Exec(ctx)

	secretKeys := map[string]bool{"password": true, "api_key": true, "auth_token": true, "api_secret": true}

	for k, v := range req.Settings {
		if v == "" || v == "••••••••" {
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
			SetValue(v).
			SetIsSecret(secretKeys[k]).
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

	jsonResponse(w, http.StatusOK, map[string]string{"message": "settings saved"})
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
	})
	r.Get("/branding", h.GetBranding)
	r.Put("/branding", h.UpdateBranding)
}
