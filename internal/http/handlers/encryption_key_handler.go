package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/platformkey"
)

const encryptionKeyDescription = "AES-256-GCM key for provider-credential encryption at rest"

// EncryptionKeyHandler exposes platform-owner management of the provider-credential
// encryption key. The raw key is NEVER returned in any response — only a fingerprint,
// its source, and metadata.
type EncryptionKeyHandler struct {
	store       *platformkey.Store
	logger      *zap.Logger
	keyProvider *encryption.KeyProvider
}

// NewEncryptionKeyHandler creates a new EncryptionKeyHandler.
func NewEncryptionKeyHandler(client *ent.Client, logger *zap.Logger, keyProvider *encryption.KeyProvider) *EncryptionKeyHandler {
	return &EncryptionKeyHandler{
		store:       platformkey.NewStore(client),
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
		if _, updated, found, err := h.store.Get(ctx, encryption.DBConfigKey); err == nil && found {
			ts := updated.Format("2006-01-02T15:04:05Z")
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
		generated, err := platformkey.Random(32)
		if err != nil {
			h.logger.Error("failed to generate encryption key")
			notifRespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to generate key"})
			return
		}
		keyBytes = generated
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
	if err := h.store.Put(ctx, encryption.DBConfigKey, encoded, encryptionKeyDescription); err != nil {
		h.logger.Error("failed to save encryption key", zap.Error(err))
		notifRespondJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save key"})
		return
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
