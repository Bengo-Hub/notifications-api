package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/providers"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
	"github.com/bengobox/notifications-api/internal/whatsapp/tenanttemplates"
)

// WhatsAppTemplates exposes platform-admin management of the drafted WhatsApp message templates —
// idempotent sync to Meta's Graph API, reusing internal/whatsapp/templatesync (the same package
// cmd/whatsapp-template-sync uses), so the CLI and this UI-facing endpoint can never drift.
type WhatsAppTemplates struct {
	manager *providers.Manager
	logger  *zap.Logger
	// tenant syncs the templates a tenant uses to the tenant's own WABA (tenant settings routes,
	// see TenantStatus and TenantSync). Nil leaves those routes unmounted.
	tenant *tenanttemplates.Service
	// deliveryLogs feeds the WhatsApp credit guide (CreditCategories); nil leaves the tenant's
	// own message list empty.
	deliveryLogs *ent.Client
}

// NewWhatsAppTemplates creates the handler. manager resolves the WABA ID + access token from the
// platform tenant's own whatsapp/meta_cloud settings — see Manager.LoadWhatsAppTemplateCredentials.
// tenant serves the per-tenant template routes.
func NewWhatsAppTemplates(manager *providers.Manager, logger *zap.Logger, tenant *tenanttemplates.Service) *WhatsAppTemplates {
	return &WhatsAppTemplates{manager: manager, logger: logger.Named("whatsapp-templates"), tenant: tenant}
}

type syncTemplatesRequest struct {
	DryRun bool     `json:"dry_run"`
	Only   []string `json:"only,omitempty"` // name prefixes, e.g. ["finance_"] — empty means every template
	// Names limits the run to these exact template names: the UI's submit queue sends one batch
	// of names per call, so a template Meta refused is not retried by every later batch.
	Names []string `json:"names,omitempty"`
	// BatchSize caps how many templates one call submits (default 5, at most 10); the rest come
	// back queued and the caller calls again until remaining is 0.
	BatchSize int `json:"batch_size,omitempty"`
}

type syncTemplatesResponse struct {
	WABAID    string                `json:"waba_id"`
	Results   []templatesync.Result `json:"results"`
	Summary   map[string]int        `json:"summary"`
	Remaining int                   `json:"remaining"`
}

const (
	defaultSyncBatch = 5
	maxSyncBatch     = 10
)

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
	if len(req.Names) > 0 {
		defs = templatesync.FilterByName(defs, req.Names)
	}

	batch := req.BatchSize
	if batch <= 0 {
		batch = defaultSyncBatch
	}
	batch = min(batch, maxSyncBatch)

	syncer := templatesync.NewSyncer(wabaID, token)
	// A batch already under way finishes even if the browser gives up waiting, so a template is
	// never half submitted; the caller's next dry run shows what reached Meta.
	results, remaining, err := syncer.RunBatch(context.WithoutCancel(ctx), defs, req.DryRun, batch)
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
		// Keep Meta's reason in the logs too: the response is the only other place it appears,
		// and a dropped connection (a 502 at the edge) lost it on 2026-10-07.
		if res.Outcome == templatesync.OutcomeFailed {
			h.logger.Warn("whatsapp template refused by Meta", zap.String("template", res.Name), zap.String("detail", res.Detail))
		}
	}

	jsonResponse(w, http.StatusOK, syncTemplatesResponse{WABAID: wabaID, Results: results, Summary: summary, Remaining: remaining})
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

// tenantTemplateTimeout bounds one tenant status or sync call to Meta. The calls run on a context
// detached from the request, so a browser that stops waiting never leaves a batch half submitted.
const tenantTemplateTimeout = 2 * time.Minute

// tenantTemplatesError answers a failed tenant template call: a missing connection is the
// caller's to fix (400), anything else is Meta's or ours (502).
func (h *WhatsAppTemplates) tenantTemplatesError(w http.ResponseWriter, tenantID, what string, err error) {
	if errors.Is(err, tenanttemplates.ErrCredentials) {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.logger.Error("tenant whatsapp templates: "+what+" failed", zap.String("tenant_id", tenantID), zap.Error(err))
	jsonError(w, http.StatusBadGateway, what+" failed: "+err.Error())
}

// TenantStatus lists each template the acting tenant uses with its review status on the tenant's
// own WhatsApp Business Account (APPROVED, PENDING, REJECTED with Meta's reason, or MISSING).
// @Summary WhatsApp template status on the tenant's own WABA
// @Tags Settings
// @Produce json
// @Success 200 {object} tenanttemplates.StatusReport
// @Failure 400,502 {object} errorResponse
// @Security bearerAuth
// @Router /settings/whatsapp/templates [get]
func (h *WhatsAppTemplates) TenantStatus(w http.ResponseWriter, r *http.Request) {
	if h.tenant == nil {
		jsonError(w, http.StatusServiceUnavailable, "tenant whatsapp templates are not configured")
		return
	}
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), tenantTemplateTimeout)
	defer cancel()
	rep, err := h.tenant.TenantTemplateStatus(ctx, tenantID)
	if err != nil {
		h.tenantTemplatesError(w, tenantID, "template status", err)
		return
	}
	jsonResponse(w, http.StatusOK, rep)
}

type tenantSyncRequest struct {
	DryRun bool `json:"dry_run"`
}

// TenantSync submits the templates the acting tenant uses that are not yet on its own WABA, at
// most tenanttemplates.BatchSize per call; the caller repeats while remaining is above 0.
// @Summary Sync WhatsApp templates to the tenant's own WABA
// @Tags Settings
// @Accept json
// @Produce json
// @Param request body tenantSyncRequest false "Sync options"
// @Success 200 {object} tenanttemplates.SyncReport
// @Failure 400,502 {object} errorResponse
// @Security bearerAuth
// @Router /settings/whatsapp/templates/sync [post]
func (h *WhatsAppTemplates) TenantSync(w http.ResponseWriter, r *http.Request) {
	if h.tenant == nil {
		jsonError(w, http.StatusServiceUnavailable, "tenant whatsapp templates are not configured")
		return
	}
	// The tenant always comes from the caller's token; X-Tenant-ID counts only for a platform owner.
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	var req tenantSyncRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // an empty body is a live sync

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), tenantTemplateTimeout)
	defer cancel()
	rep, err := h.tenant.SyncTenantTemplates(ctx, tenantID, req.DryRun)
	if err != nil {
		h.tenantTemplatesError(w, tenantID, "template sync", err)
		return
	}
	h.logger.Info("tenant whatsapp template sync",
		zap.String("tenant_id", tenantID), zap.Bool("dry_run", req.DryRun),
		zap.Any("summary", rep.Summary), zap.Int("remaining", rep.Remaining))
	jsonResponse(w, http.StatusOK, rep)
}

// RegisterTenantTemplateRoutes mounts the tenant template routes on the tenant /settings router.
// adminOnly guards both: the status call reads the tenant's WABA with its own token, and only a
// tenant admin (the role that can connect the number) may use it.
func (h *WhatsAppTemplates) RegisterTenantTemplateRoutes(r chi.Router, adminOnly func(http.Handler) http.Handler) {
	// The credit guide only reads the tenant's own sends: any settings reader may see it.
	r.Get("/whatsapp/credit-categories", h.CreditCategories)
	if h.tenant == nil {
		return
	}
	r.With(adminOnly).Get("/whatsapp/templates", h.TenantStatus)
	r.With(adminOnly).Post("/whatsapp/templates/sync", h.TenantSync)
}

// RegisterWhatsAppTemplateRoutes registers the sync endpoint under /platform.
func (h *WhatsAppTemplates) RegisterWhatsAppTemplateRoutes(r chi.Router) {
	r.Route("/whatsapp/templates", func(wa chi.Router) {
		wa.Post("/sync", h.Sync)
		wa.Delete("/", h.Delete)
	})
}
