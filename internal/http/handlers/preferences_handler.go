package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/serviceconfig"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// PreferencesHandler exposes the tenant-scoped notification-type toggles that feed the
// worker's dispatch gate (modules/preferences.Gate). Backed by ServiceConfig rows
// (tenant_id = tenant → override; the gate falls back to platform default rows and the
// registry class default: essential=ON, optional=OFF, locked=always on).
type PreferencesHandler struct {
	log    *zap.Logger
	client *ent.Client
	gate   *preferences.Gate
}

// NewPreferencesHandler builds the handler. gate may be nil (no cache invalidation).
func NewPreferencesHandler(log *zap.Logger, client *ent.Client, gate *preferences.Gate) *PreferencesHandler {
	return &PreferencesHandler{log: log.Named("notification-preferences"), client: client, gate: gate}
}

type preferenceRow struct {
	Key        string `json:"key"`
	Label      string `json:"label"`
	Group      string `json:"group"`
	Class      string `json:"class"`   // locked | essential | optional
	Default    bool   `json:"default"` // registry/platform default when no tenant override
	Enabled    bool   `json:"enabled"` // effective value for this tenant
	Overridden bool   `json:"overridden"`
}

// List returns every registered notification type with its effective per-tenant value.
// GET /api/v1/notification-preferences
func (h *PreferencesHandler) List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, ok := h.tenantUUID(r)
	if !ok {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}

	// Load all toggle rows for this tenant and the platform defaults in two queries.
	tenantRows, err := h.client.ServiceConfig.Query().
		Where(serviceconfig.TenantIDEQ(tenantID), serviceconfig.ConfigKeyHasPrefix("notifications.type.")).
		All(ctx)
	if err != nil {
		h.log.Error("list tenant notification prefs failed", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load preferences"})
		return
	}
	platformRows, err := h.client.ServiceConfig.Query().
		Where(serviceconfig.TenantIDIsNil(), serviceconfig.ConfigKeyHasPrefix("notifications.type.")).
		All(ctx)
	if err != nil {
		h.log.Error("list platform notification prefs failed", zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to load preferences"})
		return
	}
	tenantByKey := make(map[string]string, len(tenantRows))
	for _, row := range tenantRows {
		tenantByKey[row.ConfigKey] = row.ConfigValue
	}
	platformByKey := make(map[string]string, len(platformRows))
	for _, row := range platformRows {
		platformByKey[row.ConfigKey] = row.ConfigValue
	}

	out := make([]preferenceRow, 0, len(preferences.Registry))
	for _, t := range preferences.Registry {
		key := preferences.ConfigKey(t.Key)
		def := preferences.DefaultEnabled(t.Key)
		if v, ok := platformByKey[key]; ok {
			def = parsePrefBool(v, def)
		}
		enabled := def
		overridden := false
		if t.Class == preferences.ClassLocked {
			enabled = true
			def = true
		} else if v, ok := tenantByKey[key]; ok {
			enabled = parsePrefBool(v, def)
			overridden = true
		}
		out = append(out, preferenceRow{
			Key:        t.Key,
			Label:      t.Label,
			Group:      t.Group,
			Class:      string(t.Class),
			Default:    def,
			Enabled:    enabled,
			Overridden: overridden,
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": out, "total": len(out)})
}

type upsertPreferenceRequest struct {
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
}

// Upsert sets the tenant's toggle for one notification type.
// PUT /api/v1/notification-preferences  body: {"key":"finance/payment_success","enabled":false}
func (h *PreferencesHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, ok := h.tenantUUID(r)
	if !ok {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}

	var req upsertPreferenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	t, known := preferences.Lookup(req.Key)
	if !known {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown notification type"})
		return
	}
	if t.Class == preferences.ClassLocked {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "this notification is security-critical and cannot be disabled"})
		return
	}

	key := preferences.ConfigKey(req.Key)
	value := "false"
	if req.Enabled {
		value = "true"
	}

	existing, _ := h.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKeyEQ(key), serviceconfig.TenantIDEQ(tenantID)).
		First(ctx)
	var err error
	if existing != nil {
		_, err = existing.Update().SetConfigValue(value).Save(ctx)
	} else {
		_, err = h.client.ServiceConfig.Create().
			SetTenantID(tenantID).
			SetConfigKey(key).
			SetConfigValue(value).
			SetConfigType("bool").
			SetDescription(t.Label + " (notification toggle)").
			Save(ctx)
	}
	if err != nil {
		h.log.Error("upsert notification pref failed", zap.String("key", key), zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save preference"})
		return
	}

	if h.gate != nil {
		h.gate.Invalidate(ctx, tenantID.String(), req.Key)
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"key":     req.Key,
		"enabled": req.Enabled,
	})
}

// tenantUUID resolves the caller's tenant UUID from the request context, falling back
// to a slug lookup against the local tenant projection (JIT-synced by the router).
func (h *PreferencesHandler) tenantUUID(r *http.Request) (uuid.UUID, bool) {
	ctx := r.Context()
	if s := httpware.GetTenantID(ctx); s != "" {
		if id, err := uuid.Parse(s); err == nil && id != uuid.Nil {
			return id, true
		}
	}
	if slug := httpware.GetTenantSlug(ctx); slug != "" {
		if t, err := h.client.Tenant.Query().Where(enttenant.SlugEQ(slug)).First(ctx); err == nil {
			return t.ID, true
		}
	}
	return uuid.Nil, false
}

func parsePrefBool(raw string, fallback bool) bool {
	switch raw {
	case "true", "1", "TRUE", "True":
		return true
	case "false", "0", "FALSE", "False":
		return false
	default:
		return fallback
	}
}
