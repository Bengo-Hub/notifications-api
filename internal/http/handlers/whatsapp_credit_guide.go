package handlers

import (
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/deliverylog"
	"github.com/bengobox/notifications-api/internal/modules/credits"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// whatsAppGuideWindow is how far back the guide looks at the tenant's own WhatsApp sends.
const whatsAppGuideWindow = 90 * 24 * time.Hour

// WithDeliveryLog lets the handler read the tenant's sends for the WhatsApp credit guide.
func (h *WhatsAppTemplates) WithDeliveryLog(client *ent.Client) *WhatsAppTemplates {
	h.deliveryLogs = client
	return h
}

// CreditCategories tells a tenant, in plain words, which of its WhatsApp messages use which
// credits: per category the plain name, an explanation, examples, and the tenant's own message
// kinds from the last 90 days, most sent first.
// @Summary Which WhatsApp messages use which credits
// @Description One entry per WhatsApp credit category (promotions and broadcasts; order, payment and account updates; login and verification codes; replies to customers) with the tenant's own messages of the last 90 days.
// @Tags WhatsApp
// @Produce json
// @Success 200 {object} map[string]any
// @Security bearerAuth
// @Router /settings/whatsapp/credit-categories [get]
func (h *WhatsAppTemplates) CreditCategories(w http.ResponseWriter, r *http.Request) {
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}
	sent := map[string]int{}
	if h.deliveryLogs != nil {
		var rows []struct {
			TemplateID string `json:"template_id"`
			Count      int    `json:"count"`
		}
		err := h.deliveryLogs.DeliveryLog.Query().
			Where(
				deliverylog.TenantID(tenantID),
				deliverylog.Channel("whatsapp"),
				deliverylog.CreatedAtGTE(time.Now().Add(-whatsAppGuideWindow)),
			).
			GroupBy(deliverylog.FieldTemplateID).
			Aggregate(ent.Count()).
			Scan(r.Context(), &rows)
		if err != nil {
			// The categories and examples still help without the tenant's own list.
			h.logger.Warn("whatsapp credit guide: delivery log read failed", zap.String("tenant_id", tenantID), zap.Error(err))
		}
		for _, row := range rows {
			sent[row.TemplateID] += row.Count
		}
	}
	guide := credits.BuildWhatsAppGuide(sent, func(id string) string {
		if t, ok := preferences.Lookup(id); ok {
			return t.Label
		}
		return ""
	})
	jsonResponse(w, http.StatusOK, map[string]any{"categories": guide, "window_days": int(whatsAppGuideWindow.Hours() / 24)})
}
