package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/providers"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// WhatsAppTemplates exposes platform-admin management of the drafted WhatsApp message templates —
// idempotent sync to Meta's Graph API, reusing internal/whatsapp/templatesync (the same package
// cmd/whatsapp-template-sync uses), so the CLI and this UI-facing endpoint can never drift.
type WhatsAppTemplates struct {
	manager *providers.Manager
	logger  *zap.Logger
}

// NewWhatsAppTemplates creates the handler. manager resolves the WABA ID + access token from the
// platform tenant's own whatsapp/meta_cloud settings — see Manager.LoadWhatsAppTemplateCredentials.
func NewWhatsAppTemplates(manager *providers.Manager, logger *zap.Logger) *WhatsAppTemplates {
	return &WhatsAppTemplates{manager: manager, logger: logger.Named("whatsapp-templates")}
}

type syncTemplatesRequest struct {
	DryRun bool     `json:"dry_run"`
	Only   []string `json:"only,omitempty"` // name prefixes, e.g. ["finance_"] — empty means every template
}

type syncTemplatesResponse struct {
	WABAID  string                 `json:"waba_id"`
	Results []templatesync.Result  `json:"results"`
	Summary map[string]int         `json:"summary"`
}

// Sync runs the idempotent WhatsApp template sync against Meta. With dry_run true (the UI's
// default), nothing is sent to Meta — it only reports what would be created versus what already
// exists. Superuser-only (mounted under /platform).
// @Summary Sync WhatsApp templates to Meta
// @Description Idempotently creates any drafted WhatsApp template not already registered on the platform's WABA. dry_run previews with no write calls to Meta.
// @Tags Platform
// @Accept json
// @Produce json
// @Param request body syncTemplatesRequest true "Sync options"
// @Success 200 {object} syncTemplatesResponse
// @Failure 400,500 {object} errorResponse
// @Security bearerAuth
// @Router /platform/whatsapp/templates/sync [post]
func (h *WhatsAppTemplates) Sync(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		jsonError(w, http.StatusServiceUnavailable, "whatsapp template sync is not configured")
		return
	}

	var req syncTemplatesRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body is valid — defaults to a live full sync

	ctx := r.Context()
	wabaID, token, err := h.manager.LoadWhatsAppTemplateCredentials(ctx)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "cannot resolve WhatsApp template credentials: "+err.Error())
		return
	}

	defs, err := templatesync.LoadManifest()
	if err != nil {
		h.logger.Error("load template manifest", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to load template manifest")
		return
	}
	if len(req.Only) > 0 {
		defs = templatesync.FilterByPrefix(defs, req.Only)
	}

	syncer := templatesync.NewSyncer(wabaID, token)
	results, err := syncer.Run(ctx, defs, req.DryRun)
	if err != nil {
		h.logger.Error("whatsapp template sync failed", zap.Error(err))
		jsonError(w, http.StatusBadGateway, "sync failed: "+err.Error())
		return
	}

	summary := map[string]int{}
	for _, res := range results {
		key := string(res.Outcome)
		if res.DryRun && res.Outcome == templatesync.OutcomeCreated {
			key = "would_create"
		}
		summary[key]++
	}

	jsonResponse(w, http.StatusOK, syncTemplatesResponse{WABAID: wabaID, Results: results, Summary: summary})
}

// RegisterWhatsAppTemplateRoutes registers the sync endpoint under /platform.
func (h *WhatsAppTemplates) RegisterWhatsAppTemplateRoutes(r chi.Router) {
	r.Route("/whatsapp/templates", func(wa chi.Router) {
		wa.Post("/sync", h.Sync)
	})
}
