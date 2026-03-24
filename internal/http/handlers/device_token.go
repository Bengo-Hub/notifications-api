package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	httpware "github.com/Bengo-Hub/httpware"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/devicetoken"
)

// DeviceTokenHandler manages FCM/APNS device token registration.
type DeviceTokenHandler struct {
	log    *zap.Logger
	client *ent.Client
}

// NewDeviceTokenHandler creates a new DeviceTokenHandler.
func NewDeviceTokenHandler(log *zap.Logger, client *ent.Client) *DeviceTokenHandler {
	return &DeviceTokenHandler{
		log:    log.Named("device_token"),
		client: client,
	}
}

// RegisterTokenRequest is the request body for registering a device token.
type RegisterTokenRequest struct {
	Token    string `json:"token"`
	Platform string `json:"platform"` // android, ios, web
	Provider string `json:"provider"` // fcm, apns (defaults to fcm)
}

// RegisterToken registers or updates a device push notification token.
// POST /push/tokens
func (h *DeviceTokenHandler) RegisterToken(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.extractIDs(w, r)
	if !ok {
		return
	}

	var req RegisterTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		RespondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" {
		RespondError(w, http.StatusBadRequest, "token is required")
		return
	}
	if req.Platform == "" {
		req.Platform = "android"
	}
	if req.Provider == "" {
		req.Provider = "fcm"
	}

	// Upsert: if token already exists, update it (may have changed user/tenant)
	existing, _ := h.client.DeviceToken.Query().
		Where(devicetoken.Token(req.Token)).
		Only(r.Context())

	if existing != nil {
		_, err := existing.Update().
			SetTenantID(tenantID).
			SetUserID(userID).
			SetPlatform(req.Platform).
			SetProvider(req.Provider).
			SetIsActive(true).
			Save(r.Context())
		if err != nil {
			h.log.Error("failed to update device token", zap.Error(err))
			RespondError(w, http.StatusInternalServerError, "failed to update device token")
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{
			"id":      existing.ID,
			"message": "device token updated",
		})
		return
	}

	dt, err := h.client.DeviceToken.Create().
		SetTenantID(tenantID).
		SetUserID(userID).
		SetToken(req.Token).
		SetPlatform(req.Platform).
		SetProvider(req.Provider).
		SetIsActive(true).
		Save(r.Context())
	if err != nil {
		h.log.Error("failed to register device token", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to register device token")
		return
	}

	respondJSON(w, http.StatusCreated, map[string]any{
		"id":      dt.ID,
		"message": "device token registered",
	})
}

// DeleteToken deactivates a device token.
// DELETE /push/tokens
func (h *DeviceTokenHandler) DeleteToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token == "" {
		RespondError(w, http.StatusBadRequest, "token is required")
		return
	}

	count, err := h.client.DeviceToken.Update().
		Where(devicetoken.Token(req.Token)).
		SetIsActive(false).
		Save(r.Context())
	if err != nil {
		h.log.Error("failed to deactivate device token", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to deactivate device token")
		return
	}

	if count == 0 {
		RespondError(w, http.StatusNotFound, "token not found")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"message": "device token deactivated"})
}

// ListTokens returns active device tokens for the current user.
// GET /push/tokens
func (h *DeviceTokenHandler) ListTokens(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := h.extractIDs(w, r)
	if !ok {
		return
	}

	tokens, err := h.client.DeviceToken.Query().
		Where(
			devicetoken.TenantID(tenantID),
			devicetoken.UserID(userID),
			devicetoken.IsActive(true),
		).
		All(r.Context())
	if err != nil {
		h.log.Error("failed to list device tokens", zap.Error(err))
		RespondError(w, http.StatusInternalServerError, "failed to list device tokens")
		return
	}

	items := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, map[string]any{
			"id":         t.ID,
			"platform":   t.Platform,
			"provider":   t.Provider,
			"created_at": t.CreatedAt,
		})
	}

	respondJSON(w, http.StatusOK, map[string]any{"tokens": items})
}

// extractIDs extracts tenant and user IDs from request context.
func (h *DeviceTokenHandler) extractIDs(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	tenantIDStr := httpware.GetTenantID(r.Context())
	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid tenant ID")
		return uuid.Nil, uuid.Nil, false
	}

	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		RespondError(w, http.StatusUnauthorized, "authentication required")
		return uuid.Nil, uuid.Nil, false
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		RespondError(w, http.StatusBadRequest, "invalid user ID in token")
		return uuid.Nil, uuid.Nil, false
	}

	return tenantID, userID, true
}
