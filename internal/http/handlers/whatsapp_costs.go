package handlers

import (
	"errors"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/wacost"
)

// WhatsAppCostsHandler serves the platform-owner WhatsApp cost report (owner decision
// 2026-11-12): Meta's charges per WhatsApp Business Account and number next to what each tenant
// paid us in WhatsApp credits.
type WhatsAppCostsHandler struct {
	svc *wacost.Service
	log *zap.Logger
	now func() time.Time
}

// NewWhatsAppCostsHandler creates the handler.
func NewWhatsAppCostsHandler(svc *wacost.Service, log *zap.Logger) *WhatsAppCostsHandler {
	if log == nil {
		log = zap.NewNop()
	}
	return &WhatsAppCostsHandler{svc: svc, log: log.Named("whatsapp.costs"), now: time.Now}
}

// RegisterPlatformRoutes mounts GET /platform/whatsapp/costs under the platform routes.
func (h *WhatsAppCostsHandler) RegisterPlatformRoutes(r chi.Router) {
	r.Get("/whatsapp/costs", h.Get)
}

// Get godoc
// @Summary WhatsApp costs per account and number (platform owner)
// @Description Per WhatsApp Business Account we send from and per number on it: Meta's messages and cost by category (pricing_analytics, in the account's currency), the tenant's WhatsApp credits spent by category, KES paid and a margin estimate where Meta bills in KES. A WABA Meta refuses shows its error instead of failing the report. Cached for one hour per range.
// @Tags Platform
// @Produce json
// @Security BearerAuth
// @Param from query string false "First day, YYYY-MM-DD (UTC). Default: first day of this month"
// @Param to query string false "Last day, YYYY-MM-DD (UTC). Default: today"
// @Success 200 {object} wacost.Report
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /platform/whatsapp/costs [get]
func (h *WhatsAppCostsHandler) Get(w http.ResponseWriter, r *http.Request) {
	// The /platform group already requires the super admin role; the report also needs the
	// signed platform-owner claim, because it shows every tenant's money.
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims == nil || !claims.IsPlatformOwner {
		jsonError(w, http.StatusForbidden, "only platform owners can see WhatsApp costs")
		return
	}
	rng, err := wacost.ParseRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"), h.now())
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	rep, err := h.svc.Build(r.Context(), rng)
	if err != nil {
		if errors.Is(err, wacost.ErrBadRange) {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.log.Error("whatsapp cost report failed", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "the WhatsApp cost report could not be built")
		return
	}
	respondJSON(w, http.StatusOK, rep)
}
