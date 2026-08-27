package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/encryption"
)

// SettingsHandler exposes tenant-level security settings.
type SettingsHandler struct {
	log         *zap.Logger
	keyProvider *encryption.KeyProvider
}

// NewSettingsHandler creates a new SettingsHandler.
func NewSettingsHandler(log *zap.Logger, keyProvider *encryption.KeyProvider) *SettingsHandler {
	return &SettingsHandler{log: log, keyProvider: keyProvider}
}

type securitySettingsResponse struct {
	// WebhookSecret is the HMAC-derived signing secret tenants use to verify inbound webhook payloads.
	WebhookSecret string `json:"webhook_secret"`
}

// GetSecuritySettings returns the tenant's webhook signing secret.
// GET /api/v1/settings/security
func (h *SettingsHandler) GetSecuritySettings(w http.ResponseWriter, r *http.Request) {
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		http.Error(w, `{"error":"tenant_id required"}`, http.StatusBadRequest)
		return
	}

	secret := h.deriveWebhookSecret(r.Context(), tenantID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(securitySettingsResponse{
		WebhookSecret: secret,
	})
}

// deriveWebhookSecret creates a deterministic per-tenant webhook signing secret.
// Uses HMAC-SHA256(encryptionKey, tenantID+":webhook_signing_key").
// If no encryption key is configured, falls back to a SHA256 of the tenant ID (less secure).
func (h *SettingsHandler) deriveWebhookSecret(ctx context.Context, tenantID string) string {
	key := h.keyProvider.Primary(ctx)
	if len(key) == 0 {
		// No platform key configured — use tenant ID as fallback (shows unconfigured state)
		sum := sha256.Sum256([]byte(tenantID + ":webhook_signing_key"))
		return "whsec_" + hex.EncodeToString(sum[:])
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(tenantID + ":webhook_signing_key"))
	return "whsec_" + hex.EncodeToString(mac.Sum(nil))
}
