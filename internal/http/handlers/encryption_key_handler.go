package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/serviceconfig"
)

const encryptionKeyDescription = "AES-256-GCM key for provider-credential encryption at rest"

// EncryptionKeyHandler exposes platform-owner management of the provider-credential
// encryption key. The raw key is NEVER returned in any response — only a fingerprint,
// its source, and metadata.
type EncryptionKeyHandler struct {
	client      *ent.Client
	logger      *zap.Logger
	keyProvider *encryption.KeyProvider
}

// NewEncryptionKeyHandler creates a new EncryptionKeyHandler.
func NewEncryptionKeyHandler(client *ent.Client, logger *zap.Logger, keyProvider *encryption.KeyProvider) *EncryptionKeyHandler {
	return &EncryptionKeyHandler{
		client:      client,
		logger:      logger.Named("encryption-key"),
		keyProvider: keyProvider,
	}
}

type encryptionKeyResponse struct {
	Configured     bool    `json:"configured"`
	Source         string  `json:"source"` // "db" | "env" | ""
	KeyFingerprint string  `json:"key_fingerprint"`
	UpdatedAt      *string `json:"updated_at"`
}

// status builds the public response. updatedAt is the DB row's updated_at when the key
// is stored in the DB; nil otherwise (env-sourced or unset). Never includes key material.
func (h *EncryptionKeyHandler) status(r *http.Request) encryptionKeyResponse {
	ctx := r.Context()
	resp := encryptionKeyResponse{
		Configured:     h.keyProvider.Configured(ctx),
		Source:         h.keyProvider.Source(ctx),
		KeyFingerprint: h.keyProvider.Fingerprint(ctx),
	}

	if resp.Source == "db" {
		if row, err := h.client.ServiceConfig.Query().
			Where(
				serviceconfig.ConfigKeyEQ(encryption.DBConfigKey),
				serviceconfig.TenantIDIsNil(),
			).
			First(ctx); err == nil && row != nil {
			ts := row.UpdatedAt.Format("2006-01-02T15:04:05Z")
			resp.UpdatedAt = &ts
		}
	}
	return resp
}

// GetEncryptionKey returns the current encryption-key status (no key material).
// GET /api/v1/platform/encryption-key
func (h *EncryptionKeyHandler) GetEncryptionKey(w http.ResponseWriter, r *http.Request) {
	notifRespondJSON(w, http.StatusOK, h.status(r))
}

type setEncryptionKeyRequest struct {
	Key      string `json:"key,omitempty"`
	Generate bool   `json:"generate,omitempty"`
}

// SetEncryptionKey upserts the platform-level (tenant_id IS NULL) encryption key.
// PUT /api/v1/platform/encryption-key
// Body: {"key": "<base64-std of 32 bytes>"} or {"generate": true}. An empty/absent key
// (or generate=true) produces 32 cryptographically random bytes. The raw key is never
// returned and never logged.
func (h *EncryptionKeyHandler) SetEncryptionKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req setEncryptionKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		notifRespondJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	var keyBytes []byte
	if req.Generate || req.Key == "" {
		keyBytes = make([]byte, 32)
		if _, err := rand.Read(keyBytes); err != nil {
			h.logger.Error("failed to generate encryption key")
			notifRespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate key"})
			return
		}
	} else {
		decoded, err := base64.StdEncoding.DecodeString(req.Key)
		if err != nil || len(decoded) != 32 {
			notifRespondJSON(w, http.StatusBadRequest, map[string]string{"error": "key must be base64-encoded 32 bytes"})
			return
		}
		keyBytes = decoded
	}

	encoded := base64.StdEncoding.EncodeToString(keyBytes)

	// Upsert the platform-level ServiceConfig row (tenant_id IS NULL).
	existing, _ := h.client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKeyEQ(encryption.DBConfigKey),
			serviceconfig.TenantIDIsNil(),
		).
		First(ctx)

	if existing != nil {
		if _, err := existing.Update().
			SetConfigValue(encoded).
			SetConfigType("string").
			SetIsSecret(true).
			SetDescription(encryptionKeyDescription).
			Save(ctx); err != nil {
			h.logger.Error("failed to update encryption key", zap.Error(err))
			notifRespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save key"})
			return
		}
	} else {
		if _, err := h.client.ServiceConfig.Create().
			SetConfigKey(encryption.DBConfigKey).
			SetConfigValue(encoded).
			SetConfigType("string").
			SetIsSecret(true).
			SetDescription(encryptionKeyDescription).
			Save(ctx); err != nil { // tenant_id left nil => platform-level
			h.logger.Error("failed to create encryption key", zap.Error(err))
			notifRespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save key"})
			return
		}
	}

	// Drop the cached candidates so the new key takes effect immediately.
	h.keyProvider.Invalidate()

	h.logger.Info("platform encryption key updated", zap.String("source", "db"))
	notifRespondJSON(w, http.StatusOK, h.status(r))
}

// RegisterPlatformRoutes registers the encryption-key routes on the platform-gated group.
func (h *EncryptionKeyHandler) RegisterPlatformRoutes(r chi.Router) {
	r.Get("/encryption-key", h.GetEncryptionKey)
	r.Put("/encryption-key", h.SetEncryptionKey)
}
