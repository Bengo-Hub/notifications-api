package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/serviceconfig"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
	"github.com/bengobox/notifications-api/internal/platform/templates"
)

// PreferencesHandler exposes the tenant-scoped notification-type toggles that feed the
// worker's dispatch gate (modules/preferences.Gate). Backed by ServiceConfig rows
// (tenant_id = tenant → override; the gate falls back to platform default rows and the
// registry class default: essential=ON, optional=OFF, locked=always on).
type PreferencesHandler struct {
	log    *zap.Logger
	client *ent.Client
	gate   *preferences.Gate
	tpl    *templates.Loader
}

// NewPreferencesHandler builds the handler. gate may be nil (no cache invalidation). tpl is
// used to derive which channels (email/sms/whatsapp/push) each notification type actually has a
// template for, so the settings UI can group/filter by channel — nil is tolerated (Channels
// simply comes back empty).
func NewPreferencesHandler(log *zap.Logger, client *ent.Client, gate *preferences.Gate, tpl *templates.Loader) *PreferencesHandler {
	return &PreferencesHandler{log: log.Named("notification-preferences"), client: client, gate: gate, tpl: tpl}
}

type preferenceRow struct {
	Key                string   `json:"key"`
	Label              string   `json:"label"`
	Group              string   `json:"group"`
	Class              string   `json:"class"`   // locked | essential | optional
	Default            bool     `json:"default"` // registry/platform default when no tenant override
	Enabled            bool     `json:"enabled"` // effective value for this tenant
	Overridden         bool     `json:"overridden"`
	Channels           []string `json:"channels"`        // which channels (email/sms/whatsapp/push) this type HAS A TEMPLATE for (the maximum available)
	EnabledChannels    []string `json:"enabledChannels"` // which of Channels the tenant actually wants delivered — defaults to all of Channels until customized
	ChannelsOverridden bool     `json:"channelsOverridden"`
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

	channelsByKey := h.loadChannelsByKey(ctx)

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
		channels := channelsByKey[t.Key]
		if channels == nil {
			// A nil slice marshals to JSON `null`, not `[]` — the frontend iterates/joins/
			// .includes()'s this field unconditionally, so `null` would throw client-side for
			// any type with no matching template file found.
			channels = []string{}
		}

		// Enabled-channel SUBSET: tenant override -> platform override -> "every available
		// channel" (the behavior every type had before per-channel selection existed). Locked
		// types always get every channel — a tenant must not be able to silently drop the SMS
		// leg of an OTP by unchecking it, the same reasoning that already forces Enabled=true.
		enabledChannels := channels
		channelsOverridden := false
		chKey := preferences.ChannelsConfigKey(t.Key)
		if t.Class != preferences.ClassLocked {
			if v, ok := platformByKey[chKey]; ok {
				enabledChannels = intersectCSV(v, channels)
			}
			if v, ok := tenantByKey[chKey]; ok {
				enabledChannels = intersectCSV(v, channels)
				channelsOverridden = true
			}
		}

		out = append(out, preferenceRow{
			Key:                t.Key,
			Label:              t.Label,
			Group:              t.Group,
			Class:              string(t.Class),
			Default:            def,
			Enabled:            enabled,
			Overridden:         overridden,
			Channels:           channels,
			EnabledChannels:    enabledChannels,
			ChannelsOverridden: channelsOverridden,
		})
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": out, "total": len(out)})
}

// loadChannelsByKey derives, for every notification type, which channels (email/sms/whatsapp/
// push) it actually has a template file for — by listing the template directory at request time
// (cheap: a filesystem walk over a few hundred small files, not a hot path) rather than hand-
// maintaining a second parallel list that would drift from the real templates on disk. Best-
// effort: a nil loader or a listing error just means every row comes back with no channels
// (never fails the whole preferences list over this).
func (h *PreferencesHandler) loadChannelsByKey(ctx context.Context) map[string][]string {
	out := make(map[string][]string)
	if h.tpl == nil {
		return out
	}
	summaries, err := h.tpl.List(ctx)
	if err != nil {
		h.log.Warn("failed to list templates for channel derivation", zap.Error(err))
		return out
	}
	for _, s := range summaries {
		out[s.ID] = append(out[s.ID], s.Channel)
	}
	return out
}

type upsertPreferenceRequest struct {
	Key      string    `json:"key"`
	Enabled  *bool     `json:"enabled,omitempty"`
	Channels *[]string `json:"channels,omitempty"`
}

// upsertConfigValue creates or updates one ServiceConfig row for (tenantID, key).
func (h *PreferencesHandler) upsertConfigValue(ctx context.Context, tenantID uuid.UUID, key, value, configType, description string) error {
	existing, _ := h.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKeyEQ(key), serviceconfig.TenantIDEQ(tenantID)).
		First(ctx)
	if existing != nil {
		_, err := existing.Update().SetConfigValue(value).Save(ctx)
		return err
	}
	_, err := h.client.ServiceConfig.Create().
		SetTenantID(tenantID).
		SetConfigKey(key).
		SetConfigValue(value).
		SetConfigType(configType).
		SetDescription(description).
		Save(ctx)
	return err
}

// Upsert sets the tenant's toggle and/or channel selection for one notification type. Either
// field may be omitted to leave it untouched — the channel-selection modal sends only
// {key, channels}, the simple on/off switch sends only {key, enabled}.
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

	if req.Enabled != nil {
		value := "false"
		if *req.Enabled {
			value = "true"
		}
		if err := h.upsertConfigValue(ctx, tenantID, preferences.ConfigKey(req.Key), value, "bool", t.Label+" (notification toggle)"); err != nil {
			h.log.Error("upsert notification pref failed", zap.String("key", req.Key), zap.Error(err))
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save preference"})
			return
		}
	}

	if req.Channels != nil {
		available := h.loadChannelsByKey(ctx)[req.Key]
		availableSet := make(map[string]bool, len(available))
		for _, c := range available {
			availableSet[c] = true
		}
		for _, c := range *req.Channels {
			if !availableSet[c] {
				respondJSON(w, http.StatusBadRequest, map[string]string{"error": "channel \"" + c + "\" has no template for this notification type"})
				return
			}
		}
		if err := h.upsertConfigValue(ctx, tenantID, preferences.ChannelsConfigKey(req.Key), strings.Join(*req.Channels, ","), "string", t.Label+" (enabled channels)"); err != nil {
			h.log.Error("upsert notification pref channels failed", zap.String("key", req.Key), zap.Error(err))
			respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save channel selection"})
			return
		}
	}

	if h.gate != nil {
		h.gate.Invalidate(ctx, tenantID.String(), req.Key)
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"key":      req.Key,
		"enabled":  req.Enabled,
		"channels": req.Channels,
	})
}

// Reset clears the tenant's overrides (both the enabled toggle and the channel selection) for
// one notification type, reverting it to the platform default / registry default.
// DELETE /api/v1/notification-preferences/{key}
func (h *PreferencesHandler) Reset(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID, ok := h.tenantUUID(r)
	if !ok {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant context required"})
		return
	}
	key := chi.URLParam(r, "*")
	t, known := preferences.Lookup(key)
	if !known {
		respondJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown notification type"})
		return
	}

	_, err := h.client.ServiceConfig.Delete().
		Where(
			serviceconfig.TenantIDEQ(tenantID),
			serviceconfig.ConfigKeyIn(preferences.ConfigKey(key), preferences.ChannelsConfigKey(key)),
		).
		Exec(ctx)
	if err != nil {
		h.log.Error("reset notification pref failed", zap.String("key", key), zap.Error(err))
		respondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to reset preference"})
		return
	}

	if h.gate != nil {
		h.gate.Invalidate(ctx, tenantID.String(), key)
	}

	respondJSON(w, http.StatusOK, map[string]string{"key": key, "label": t.Label, "status": "reset"})
}

// tenantUUID resolves the tenant UUID this request should act on — honoring the platform-admin
// tenant-switcher (X-Tenant-ID header, then legacy ?tenantId=) via resolveActingTenantID, then
// falling back to a slug lookup against the local tenant projection (JIT-synced by the router).
func (h *PreferencesHandler) tenantUUID(r *http.Request) (uuid.UUID, bool) {
	ctx := r.Context()
	if s := resolveActingTenantID(r); s != "" {
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

// intersectCSV parses a comma-separated channel list and returns only the values that are
// also in available — guards against a stale override naming a channel this type no longer
// has a template for (e.g. a template was removed after the override was saved).
func intersectCSV(csv string, available []string) []string {
	want := map[string]bool{}
	for _, c := range strings.Split(csv, ",") {
		if c = strings.TrimSpace(c); c != "" {
			want[c] = true
		}
	}
	out := make([]string, 0, len(available))
	for _, c := range available {
		if want[c] {
			out = append(out, c)
		}
	}
	return out
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
