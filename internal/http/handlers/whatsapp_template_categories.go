package handlers

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// templateCategoryRow is one template on a WhatsApp Business Account with the category we declare
// for it (the manifest's, empty for a template we do not ship) and the one Meta bills it under.
type templateCategoryRow struct {
	Name             string `json:"name"`
	Status           string `json:"status"`
	DeclaredCategory string `json:"declared_category,omitempty"`
	MetaCategory     string `json:"meta_category"`
	PreviousCategory string `json:"previous_category,omitempty"`
	Recategorized    bool   `json:"recategorized"`
}

type templateCategoryWABA struct {
	WABAID    string                `json:"waba_id"`
	Tenants   []string              `json:"tenant_ids"`
	Error     string                `json:"error,omitempty"`
	Summary   map[string]int        `json:"summary"`
	Templates []templateCategoryRow `json:"templates"`
}

type templateCategoriesResponse struct {
	WABAs []templateCategoryWABA `json:"wabas"`
}

// Categories lists every template on every WhatsApp Business Account we send from with the
// category Meta approved it under. A template Meta moved to another category than ours (most
// often UTILITY to MARKETING) is billed and rate limited as that category, so recategorized rows
// are the ones to rewrite. Read only. Platform owner only (mounted under /platform).
// @Summary WhatsApp template categories on Meta, per WABA
// @Description Every template on each WhatsApp Business Account we send from, with our declared category, Meta's approved category and previous_category. recategorized marks templates Meta bills under another category than ours. Add ?recategorized=true to list only those.
// @Tags Platform
// @Produce json
// @Param recategorized query bool false "Only templates Meta moved to another category"
// @Success 200 {object} templateCategoriesResponse
// @Failure 503 {object} errorResponse
// @Security bearerAuth
// @Router /platform/whatsapp/templates/categories [get]
func (h *WhatsAppTemplates) Categories(w http.ResponseWriter, r *http.Request) {
	if h.manager == nil {
		jsonError(w, http.StatusServiceUnavailable, "whatsapp templates are not configured")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), tenantTemplateTimeout)
	defer cancel()

	accounts, err := h.manager.WhatsAppCostAccounts(ctx)
	if err != nil {
		jsonError(w, http.StatusBadGateway, "could not list WhatsApp accounts: "+err.Error())
		return
	}
	defs, err := templatesync.LoadManifest()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "could not load the template manifest: "+err.Error())
		return
	}
	declared := make(map[string]string, len(defs))
	for _, d := range defs {
		declared[d.Name] = d.Category
	}
	onlyMoved := r.URL.Query().Get("recategorized") == "true"

	// One WABA can hold several numbers (managed links share the platform's); read it once, with
	// the first token that is saved for it.
	type wabaCreds struct{ token, version string }
	order := []string{}
	creds := map[string]wabaCreds{}
	tenants := map[string][]string{}
	for _, a := range accounts {
		if a.WABAID == "" {
			continue
		}
		if _, seen := creds[a.WABAID]; !seen {
			order = append(order, a.WABAID)
			creds[a.WABAID] = wabaCreds{}
		}
		if c := creds[a.WABAID]; c.token == "" && a.AccessToken != "" {
			creds[a.WABAID] = wabaCreds{token: a.AccessToken, version: a.APIVersion}
		}
		if a.TenantID != "" {
			tenants[a.WABAID] = append(tenants[a.WABAID], a.TenantID)
		}
	}

	out := templateCategoriesResponse{WABAs: make([]templateCategoryWABA, 0, len(order))}
	for _, waba := range order {
		row := templateCategoryWABA{WABAID: waba, Tenants: tenants[waba], Summary: map[string]int{}, Templates: []templateCategoryRow{}}
		c := creds[waba]
		if c.token == "" {
			row.Error = "no access token is saved for this WhatsApp account"
			out.WABAs = append(out.WABAs, row)
			continue
		}
		sy := templatesync.NewSyncer(waba, c.token)
		sy.APIVersion = c.version
		existing, err := sy.FetchExisting(ctx)
		if err != nil {
			h.logger.Warn("whatsapp template categories: Meta refused the template list", zap.String("waba_id", waba), zap.Error(err))
			row.Error = err.Error()
			out.WABAs = append(out.WABAs, row)
			continue
		}
		for name, mt := range existing {
			t := templateCategoryRow{
				Name: name, Status: mt.Status, DeclaredCategory: declared[name],
				MetaCategory: mt.Category, PreviousCategory: mt.PreviousCategory,
				Recategorized: templatesync.Recategorized(declared[name], mt),
			}
			row.Summary[strings.ToLower(mt.Category)]++
			if t.Recategorized {
				row.Summary["recategorized"]++
			}
			if onlyMoved && !t.Recategorized {
				continue
			}
			row.Templates = append(row.Templates, t)
		}
		sort.Slice(row.Templates, func(i, j int) bool { return row.Templates[i].Name < row.Templates[j].Name })
		out.WABAs = append(out.WABAs, row)
	}
	jsonResponse(w, http.StatusOK, out)
}
