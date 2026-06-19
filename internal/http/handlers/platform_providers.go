package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/providers"
)

const platformTenantID = "platform"

// PlatformProviders handles platform-level notification provider configuration.
type PlatformProviders struct {
	client      *ent.Client
	logger      *zap.Logger
	keyProvider *encryption.KeyProvider
	manager     *providers.Manager
}

// NewPlatformProviders creates a new PlatformProviders handler. keyProvider resolves the
// provider-credential encryption key (DB-first, env fallback) for encrypting secrets at
// rest. manager is optional, used for the test connection endpoint.
func NewPlatformProviders(client *ent.Client, logger *zap.Logger, keyProvider *encryption.KeyProvider, manager *providers.Manager) *PlatformProviders {
	return &PlatformProviders{client: client, logger: logger, keyProvider: keyProvider, manager: manager}
}

type testProviderRequest struct {
	To string `json:"to"` // Email or phone for test message
}

type providerResponse struct {
	ID           int    `json:"id"`
	ProviderType string `json:"provider_type"` // email, sms, push
	ProviderName string `json:"provider_name"` // smtp, sendgrid, twilio, etc.
	Environment  string `json:"environment"`   // sandbox, production
	IsActive     bool   `json:"is_active"`
	Status       string `json:"status"`
}

type configureProviderRequest struct {
	ProviderType        string            `json:"provider_type"` // email, sms, push
	ProviderName        string            `json:"provider_name"` // smtp, sendgrid, twilio, etc.
	Environment         string            `json:"environment"`   // sandbox, production
	Settings            map[string]string `json:"settings"`
	PlatformManagedKeys []string          `json:"platform_managed_keys,omitempty"`
	SecretKeys          []string          `json:"secret_keys,omitempty"`
	IsPrimary           bool              `json:"is_primary,omitempty"`
}

type updateProviderRequest struct {
	Settings map[string]string `json:"settings,omitempty"`
	IsActive *bool             `json:"is_active,omitempty"`
}

// ListProviders lists platform notification providers.
func (h *PlatformProviders) ListProviders(w http.ResponseWriter, r *http.Request) {
	settings, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID(platformTenantID),
			providersetting.IsPlatform(true),
			providersetting.KeyEQ("_config"),
		).
		Order(ent.Asc(providersetting.FieldEnvironment)).
		All(r.Context())
	if err != nil {
		h.logger.Error("failed to list platform providers", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list providers")
		return
	}

	result := make([]providerResponse, 0, len(settings))
	for _, s := range settings {
		result = append(result, providerResponse{
			ID:           s.ID,
			ProviderType: s.ProviderType,
			ProviderName: s.ProviderName,
			Environment:  s.Environment,
			IsActive:     s.IsActive,
			Status:       s.Status,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]any{"providers": result})
}

// ConfigureProvider creates or updates a platform notification provider.
func (h *PlatformProviders) ConfigureProvider(w http.ResponseWriter, r *http.Request) {
	var req configureProviderRequest
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

	ctx := r.Context()

	// Fetch existing settings before delete so we can preserve values not supplied or sent empty
	existingRows, _ := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID(platformTenantID),
			providersetting.IsPlatform(true),
			providersetting.EnvironmentEQ(req.Environment),
			providersetting.ProviderType(req.ProviderType),
			providersetting.ProviderName(req.ProviderName),
			providersetting.KeyNEQ("_config"),
		).
		All(ctx)

	type existingVal struct {
		Value       string
		IsEncrypted bool
		IsSecret    bool
		IsManaged   bool
	}
	existing := make(map[string]existingVal, len(existingRows))
	for _, row := range existingRows {
		existing[row.Key] = existingVal{
			Value:       row.Value,
			IsEncrypted: row.IsEncrypted,
			IsSecret:    row.IsSecret,
			IsManaged:   row.IsPlatformManaged,
		}
	}

	// Delete existing settings for this provider + environment
	_, _ = h.client.ProviderSetting.Delete().
		Where(
			providersetting.TenantID(platformTenantID),
			providersetting.IsPlatform(true),
			providersetting.EnvironmentEQ(req.Environment),
			providersetting.ProviderType(req.ProviderType),
			providersetting.ProviderName(req.ProviderName),
		).
		Exec(ctx)

	// Create _config marker row
	_, err := h.client.ProviderSetting.Create().
		SetTenantID(platformTenantID).
		SetChannel(req.ProviderType).
		SetProvider(req.ProviderName).
		SetProviderType(req.ProviderType).
		SetProviderName(req.ProviderName).
		SetEnvironment(req.Environment).
		SetKey("_config").
		SetValue("configured").
		SetIsPlatform(true).
		SetIsActive(true).
		SetStatus("active").
		Save(ctx)
	if err != nil {
		h.logger.Error("failed to create provider config marker", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to configure provider")
		return
	}

	// Helper maps for key flags
	isManaged := make(map[string]bool)
	for _, k := range req.PlatformManagedKeys {
		isManaged[k] = true
	}
	isSecretMap := make(map[string]bool)
	for _, k := range req.SecretKeys {
		isSecretMap[k] = true
	}

	// Track which keys we've processed from the request
	processed := make(map[string]bool, len(req.Settings))

	// Create individual setting rows; encrypt secret keys when encryption key is set
	for k, v := range req.Settings {
		processed[k] = true
		value := v
		isEncrypted := false
		secret := isSecretMap[k] || encryption.IsSecret(k)

		// For empty or masked values, preserve existing DB value
		if v == "" || v == "••••••••" {
			if old, ok := existing[k]; ok && old.Value != "" {
				value = old.Value
				isEncrypted = old.IsEncrypted
				secret = secret || old.IsSecret
			} else {
				continue
			}
		} else if secret {
			if enc, ok, encErr := h.keyProvider.Encrypt(ctx, v); encErr == nil && ok {
				value = enc
				isEncrypted = true
			}
		}

		managed := isManaged[k]
		if old, ok := existing[k]; ok {
			managed = managed || old.IsManaged
		}

		_, err := h.client.ProviderSetting.Create().
			SetTenantID(platformTenantID).
			SetChannel(req.ProviderType).
			SetProvider(req.ProviderName).
			SetProviderType(req.ProviderType).
			SetProviderName(req.ProviderName).
			SetEnvironment(req.Environment).
			SetKey(k).
			SetValue(value).
			SetIsEncrypted(isEncrypted).
			SetIsPlatform(true).
			SetIsPlatformManaged(managed).
			SetIsSecret(secret).
			SetIsActive(true).
			SetStatus("active").
			Save(ctx)
		if err != nil {
			h.logger.Error("failed to save provider setting",
				zap.String("key", k),
				zap.Error(err),
			)
		}
	}

	// Re-create existing settings that were not in the request (preserve untouched keys)
	for k, old := range existing {
		if processed[k] || old.Value == "" {
			continue
		}
		_, err := h.client.ProviderSetting.Create().
			SetTenantID(platformTenantID).
			SetChannel(req.ProviderType).
			SetProvider(req.ProviderName).
			SetProviderType(req.ProviderType).
			SetProviderName(req.ProviderName).
			SetEnvironment(req.Environment).
			SetKey(k).
			SetValue(old.Value).
			SetIsEncrypted(old.IsEncrypted).
			SetIsPlatform(true).
			SetIsPlatformManaged(old.IsManaged).
			SetIsSecret(old.IsSecret).
			SetIsActive(true).
			SetStatus("active").
			Save(ctx)
		if err != nil {
			h.logger.Error("failed to re-create existing setting",
				zap.String("key", k),
				zap.Error(err),
			)
		}
	}

	h.logger.Info("platform provider configured",
		zap.String("type", req.ProviderType),
		zap.String("name", req.ProviderName),
	)

	jsonResponse(w, http.StatusCreated, map[string]string{
		"message":       "provider configured",
		"provider_type": req.ProviderType,
		"provider_name": req.ProviderName,
	})
}

// UpdateProvider updates a platform provider's settings or active state.
func (h *PlatformProviders) UpdateProvider(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	if idStr == "" {
		jsonError(w, http.StatusBadRequest, "provider ID required")
		return
	}

	var req updateProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx := r.Context()

	// Get the config marker to identify provider type/name
	id := 0
	for _, c := range idStr {
		id = id*10 + int(c-'0')
	}

	setting, err := h.client.ProviderSetting.Get(ctx, id)
	if err != nil {
		jsonError(w, http.StatusNotFound, "provider not found")
		return
	}

	if req.IsActive != nil {
		// Update all settings for this provider
		_, err := h.client.ProviderSetting.Update().
			Where(
				providersetting.TenantID(platformTenantID),
				providersetting.ProviderType(setting.ProviderType),
				providersetting.ProviderName(setting.ProviderName),
			).
			SetIsActive(*req.IsActive).
			Save(ctx)
		if err != nil {
			h.logger.Error("failed to update provider active state", zap.Error(err))
			jsonError(w, http.StatusInternalServerError, "failed to update provider")
			return
		}

		status := "active"
		if !*req.IsActive {
			status = "inactive"
		}
		_, _ = h.client.ProviderSetting.Update().
			Where(
				providersetting.TenantID(platformTenantID),
				providersetting.ProviderType(setting.ProviderType),
				providersetting.ProviderName(setting.ProviderName),
				providersetting.KeyEQ("_config"),
			).
			SetStatus(status).
			Save(ctx)
	}

	jsonResponse(w, http.StatusOK, map[string]string{"message": "provider updated"})
}

// DeactivateProvider deactivates a platform provider.
func (h *PlatformProviders) DeactivateProvider(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id := 0
	for _, c := range idStr {
		id = id*10 + int(c-'0')
	}

	ctx := r.Context()
	setting, err := h.client.ProviderSetting.Get(ctx, id)
	if err != nil {
		jsonError(w, http.StatusNotFound, "provider not found")
		return
	}

	_, err = h.client.ProviderSetting.Update().
		Where(
			providersetting.TenantID(platformTenantID),
			providersetting.ProviderType(setting.ProviderType),
			providersetting.ProviderName(setting.ProviderName),
		).
		SetIsActive(false).
		SetStatus("inactive").
		Save(ctx)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to deactivate provider")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"message": "provider deactivated"})
}

// TestProvider sends a test notification via the specified provider. Body may include {"to": "email@example.com"} or {"to": "+254700000000"}.
func (h *PlatformProviders) TestProvider(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id := 0
	for _, c := range idStr {
		if c < '0' || c > '9' {
			jsonError(w, http.StatusBadRequest, "invalid provider id")
			return
		}
		id = id*10 + int(c-'0')
	}

	ctx := r.Context()
	setting, err := h.client.ProviderSetting.Get(ctx, id)
	if err != nil {
		jsonError(w, http.StatusNotFound, "provider not found")
		return
	}
	if setting.TenantID != platformTenantID || !setting.IsPlatform {
		jsonError(w, http.StatusNotFound, "provider not found")
		return
	}

	var req testProviderRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	to := strings.TrimSpace(req.To)
	if (setting.ProviderType == "email" || setting.ProviderType == "sms") && to == "" {
		jsonError(w, http.StatusBadRequest, "test endpoint requires \"to\" (email or phone) in request body")
		return
	}

	if h.manager == nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"success":       true,
			"provider_type": setting.ProviderType,
			"provider_name": setting.ProviderName,
			"message":       "provider test skipped (no manager configured)",
		})
		return
	}

	if err := h.manager.TestConnection(ctx, setting.ProviderType, setting.ProviderName, to); err != nil {
		h.logger.Warn("provider test failed", zap.String("provider", setting.ProviderName), zap.Error(err))
		jsonResponse(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   err.Error(),
			"message": "test connection failed",
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"success":       true,
		"provider_type": setting.ProviderType,
		"provider_name": setting.ProviderName,
		"message":       "test message sent successfully",
	})
}

// RegisterPlatformProviderRoutes registers platform provider routes.
func (h *PlatformProviders) RegisterPlatformProviderRoutes(r chi.Router) {
	r.Get("/providers", h.ListProviders)
	r.Post("/providers", h.ConfigureProvider)
	r.Patch("/providers/{id}", h.UpdateProvider)
	r.Post("/providers/{id}/test", h.TestProvider)
	r.Delete("/providers/{id}", h.DeactivateProvider)
	r.Get("/providers/settings", h.GetPlatformProviderSettings)
	r.Get("/tenants", h.ListTenants)
}

type tenantListItem struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// ListTenants returns all active tenants (id, name, slug) for platform owners.
// @Summary List all tenants
// @Description Returns a lightweight list of active tenants for use in selectors (e.g. test-send tenant picker).
// @Tags Platform
// @Produce json
// @Success 200 {array} tenantListItem
// @Failure 500 {object} errorResponse
// @Security bearerAuth
// @Router /platform/tenants [get]
func (h *PlatformProviders) ListTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.client.Tenant.Query().
		Where(enttenant.StatusEQ("active")).
		Order(enttenant.ByName()).
		All(r.Context())
	if err != nil {
		h.logger.Error("failed to list tenants", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list tenants")
		return
	}

	items := make([]tenantListItem, len(tenants))
	for i, t := range tenants {
		items[i] = tenantListItem{
			ID:   t.ID.String(),
			Name: t.Name,
			Slug: t.Slug,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// GetPlatformProviderSettings returns the settings for a specific platform provider.
// Unlike the tenant endpoint, this always queries under tenant_id='platform'.
func (h *PlatformProviders) GetPlatformProviderSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	providerType := r.URL.Query().Get("provider_type")
	providerName := r.URL.Query().Get("provider_name")
	if providerType == "" || providerName == "" {
		jsonError(w, http.StatusBadRequest, "provider_type and provider_name are required")
		return
	}

	settings, err := h.client.ProviderSetting.Query().
		Where(
			providersetting.TenantID(platformTenantID),
			providersetting.ProviderType(providerType),
			providersetting.ProviderName(providerName),
			providersetting.IsPlatform(true),
			providersetting.IsActive(true),
			providersetting.KeyNEQ("_config"),
			providersetting.KeyNEQ("_preferred"),
		).
		All(ctx)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to load settings")
		return
	}

	secretKeys := map[string]bool{"password": true, "api_key": true, "auth_token": true, "api_secret": true, "service_account": true}
	result := make(map[string]string)
	for _, s := range settings {
		if secretKeys[s.Key] || s.IsSecret || s.IsEncrypted {
			if s.Value != "" {
				result[s.Key] = "••••••••"
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

