package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/billing"
)

// WhatsAppSubscriptionHandler handles WhatsApp subscription management.
type WhatsAppSubscriptionHandler struct {
	log     *zap.Logger
	service *billing.WhatsAppSubscriptionService
}

// NewWhatsAppSubscriptionHandler creates a new WhatsApp subscription handler.
func NewWhatsAppSubscriptionHandler(log *zap.Logger, service *billing.WhatsAppSubscriptionService) *WhatsAppSubscriptionHandler {
	return &WhatsAppSubscriptionHandler{
		log:     log.Named("whatsapp.subscription.handler"),
		service: service,
	}
}

// ListPlans returns all active WhatsApp subscription plans (public endpoint).
func (h *WhatsAppSubscriptionHandler) ListPlans(w http.ResponseWriter, r *http.Request) {
	plans, err := h.service.GetActivePlans(r.Context())
	if err != nil {
		h.log.Error("list plans failed", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list plans")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"data": plans})
}

// GetSubscription returns the current subscription for the authenticated tenant.
func (h *WhatsAppSubscriptionHandler) GetSubscription(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantIDStr := resolveActingTenantID(r)
	if tenantIDStr == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	sub, err := h.service.GetTenantSubscription(ctx, tenantID)
	if err != nil {
		h.log.Error("get subscription failed", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to get subscription")
		return
	}

	if sub == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"subscription": nil, "status": "none"})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"subscription": sub})
}

// Subscribe initiates a Treasury payment for a WhatsApp subscription plan.
func (h *WhatsAppSubscriptionHandler) Subscribe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantIDStr := resolveActingTenantID(r)
	if tenantIDStr == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	var req struct {
		PlanID    string `json:"plan_id"`
		ReturnURL string `json:"return_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	planID, err := uuid.Parse(req.PlanID)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid plan_id")
		return
	}

	result, err := h.service.InitiateSubscription(ctx, tenantID, planID, req.ReturnURL)
	if err != nil {
		h.log.Error("initiate subscription failed", zap.Error(err), zap.String("tenant_id", tenantIDStr))
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, result)
}

// Cancel cancels the tenant's active WhatsApp subscription.
func (h *WhatsAppSubscriptionHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantIDStr := resolveActingTenantID(r)
	if tenantIDStr == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}

	tenantID, err := uuid.Parse(tenantIDStr)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid tenant_id")
		return
	}

	if err := h.service.CancelSubscription(ctx, tenantID); err != nil {
		h.log.Error("cancel subscription failed", zap.Error(err), zap.String("tenant_id", tenantIDStr))
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"message": "subscription cancelled"})
}
