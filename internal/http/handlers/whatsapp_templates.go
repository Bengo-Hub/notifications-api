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
	WABAID  string                `json:"waba_id"`
	Results []templatesync.Result `json:"results"`
	Summary map[string]int        `json:"summary"`
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

type deleteTemplatesRequest struct {
	DryRun   bool     `json:"dry_run"`
	Prefixes []string `json:"prefixes"` // required — refuses to run with none, see Delete
}

type deleteTemplatesResponse struct {
	WABAID  string                `json:"waba_id"`
	Results []templatesync.Result `json:"results"`
	Summary map[string]int        `json:"summary"`
}

// Delete permanently removes every template on the platform's WABA (any review status) whose
// name starts with one of the given prefixes — fetched live from Meta, not the local manifest, so
// it also reaches a template a prior manifest revision created and has since renamed or dropped.
// Superuser-only (mounted under /platform), same as Sync. Requires at least one explicit prefix —
// there is deliberately no "delete everything" default, since a template still being sent (this
// platform's own fallback path included) breaks the instant it's gone.
// @Summary Delete WhatsApp templates from Meta by name prefix
// @Description Permanently deletes every template on the WABA whose name starts with one of the given prefixes. dry_run previews with no write calls to Meta.
// @Tags Platform
// @Accept json
// @Produce json
// @Param request body deleteTemplatesRequest true "Delete options"
// @Success 200 {object} deleteTemplatesResponse
// @Failure 400,500 {object} errorResponse
// @Security bearerAuth
// @Router /platform/whatsapp/templates [delete]
func (h *WhatsAppTemplates) Delete(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		jsonError(w, http.StatusServiceUnavailable, "whatsapp template sync is not configured")
		return
	}

	var req deleteTemplatesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Prefixes) == 0 {
		jsonError(w, http.StatusBadRequest, "prefixes is required and must be non-empty")
		return
	}

	ctx := r.Context()
	wabaID, token, err := h.manager.LoadWhatsAppTemplateCredentials(ctx)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "cannot resolve WhatsApp template credentials: "+err.Error())
		return
	}

	syncer := templatesync.NewSyncer(wabaID, token)
	results, err := syncer.DeleteByPrefix(ctx, req.Prefixes, req.DryRun)
	if err != nil {
		h.logger.Error("whatsapp template delete failed", zap.Error(err))
		jsonError(w, http.StatusBadGateway, "delete failed: "+err.Error())
		return
	}

	summary := map[string]int{}
	for _, res := range results {
		key := string(res.Outcome)
		if res.DryRun && res.Outcome == templatesync.OutcomeDeleted {
			key = "would_delete"
		}
		summary[key]++
	}

	jsonResponse(w, http.StatusOK, deleteTemplatesResponse{WABAID: wabaID, Results: results, Summary: summary})
}

// RegisterWhatsAppTemplateRoutes registers the sync endpoint under /platform.
func (h *WhatsAppTemplates) RegisterWhatsAppTemplateRoutes(r chi.Router) {
	r.Route("/whatsapp/templates", func(wa chi.Router) {
		wa.Post("/sync", h.Sync)
		wa.Delete("/", h.Delete)
	})
}
