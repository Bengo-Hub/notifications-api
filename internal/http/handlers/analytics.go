package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"
	"github.com/Bengo-Hub/httpware/pii"
	authclient "github.com/Bengo-Hub/shared-auth-client"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/deliverylog"
	"github.com/bengobox/notifications-api/internal/ent/predicate"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
)

// AnalyticsHandler provides delivery analytics endpoints (backed by delivery_log store).
type AnalyticsHandler struct {
	client *ent.Client
	log    *zap.Logger
}

// NewAnalyticsHandler creates a new AnalyticsHandler.
func NewAnalyticsHandler(client *ent.Client, log *zap.Logger) *AnalyticsHandler {
	return &AnalyticsHandler{client: client, log: log}
}

// DeliveryStatsResponse matches the notifications-ui DeliveryStats type.
//
// TotalSent counts messages a provider accepted (sent or delivered). Failed and Skipped are
// reported separately; skipped messages (no credit, no valid recipient, switched off) were never
// attempted, so they are left out of both rates.
type DeliveryStatsResponse struct {
	TotalSent        int               `json:"totalSent"`
	Failed           int               `json:"failed"`
	Skipped          int               `json:"skipped"`
	DeliveryRate     float64           `json:"deliveryRate"`
	ErrorRate        float64           `json:"errorRate"`
	ChannelBreakdown map[string]int    `json:"channelBreakdown"`
	TimeSeries       []TimeSeriesPoint `json:"timeSeries"`
	Scope            string            `json:"scope"` // "tenant" or "platform"
}

type TimeSeriesPoint struct {
	Date      string `json:"date"`
	Sent      int    `json:"sent"`
	Delivered int    `json:"delivered"`
}

// analyticsScope is which delivery_log rows a request may read: one tenant's, or every tenant's
// for a platform owner who asked for the platform-wide view (?scope=all).
type analyticsScope struct {
	all     bool
	tenants []string // tenant UUID, plus its slug for rows logged before UUIDs were enforced
}

func (s analyticsScope) predicate() predicate.DeliveryLog {
	if s.all {
		return func(*sql.Selector) {}
	}
	return deliverylog.TenantIDIn(s.tenants...)
}

func (s analyticsScope) name() string {
	if s.all {
		return "platform"
	}
	return "tenant"
}

// resolveScope picks the tenant for an analytics request. The {tenantId} path param and
// ?scope=all are honoured only for platform owners; anyone else always reads their own tenant.
func (h *AnalyticsHandler) resolveScope(r *http.Request) analyticsScope {
	ctx := r.Context()
	platformOwner := httpware.IsPlatformOwner(ctx)
	tenantID := ""
	if platformOwner {
		// The UI asks for scope=all only when no tenant is picked in the switcher (it still sends
		// the owner's own tenant header on business pages, so the header is not a signal here).
		if r.URL.Query().Get("scope") == "all" && chi.URLParam(r, "tenantId") == "" {
			return analyticsScope{all: true}
		}
		tenantID = chi.URLParam(r, "tenantId")
	}
	if tenantID == "" {
		tenantID = resolveActingTenantID(r)
	}
	if tenantID == "" {
		if claims, ok := authclient.ClaimsFromContext(ctx); ok {
			tenantID = claims.TenantID
		}
	}
	return analyticsScope{tenants: h.tenantKeys(ctx, tenantID)}
}

// tenantKeys returns the UUID and slug a tenant's delivery_log rows may be stored under. The HTTP
// enqueue path used to log the slug while the worker logged the UUID; both are matched so older
// rows still count.
func (h *AnalyticsHandler) tenantKeys(ctx context.Context, tenantID string) []string {
	keys := []string{tenantID}
	if h.client == nil || tenantID == "" {
		return keys
	}
	if id, err := uuid.Parse(tenantID); err == nil {
		if t, err := h.client.Tenant.Query().Where(enttenant.ID(id)).Select(enttenant.FieldSlug).Only(ctx); err == nil && t.Slug != "" {
			keys = append(keys, t.Slug)
		}
		return keys
	}
	if t, err := h.client.Tenant.Query().Where(enttenant.Slug(tenantID)).Only(ctx); err == nil {
		keys = append(keys, t.ID.String())
	}
	return keys
}

// Delivery returns delivery stats for a tenant (or the whole platform) from delivery_log.
//
// @Summary      Delivery analytics
// @Description  Returns delivery statistics for the tenant. Query: range (24h, 7d, 30d). Platform owners may pass a tenant (X-Tenant-ID or the path) or scope=all for every tenant.
// @Tags         Analytics
// @Param        tenantId  path      string  false  "Tenant identifier"
// @Param        range     query     string  false  "Time range (24h, 7d, 30d)"
// @Param        scope     query     string  false  "all = platform-wide (platform owners only)"
// @Success      200       {object}  DeliveryStatsResponse
// @Router       /analytics/delivery [get]
// @Router       /analytics/delivery/{tenantId} [get]
// @Security     bearerAuth
// @Security     ApiKeyAuth
func (h *AnalyticsHandler) Delivery(w http.ResponseWriter, r *http.Request) {
	scope := h.resolveScope(r)
	since := time.Now().Add(-24 * time.Hour)
	switch r.URL.Query().Get("range") {
	case "7d":
		since = time.Now().Add(-7 * 24 * time.Hour)
	case "30d":
		since = time.Now().Add(-30 * 24 * time.Hour)
	}

	resp := DeliveryStatsResponse{
		ChannelBreakdown: map[string]int{"email": 0, "sms": 0, "whatsapp": 0, "push": 0},
		TimeSeries:       []TimeSeriesPoint{},
		Scope:            scope.name(),
	}
	if h.client == nil {
		writeAnalyticsJSON(w, resp)
		return
	}

	ctx := r.Context()
	base := h.client.DeliveryLog.Query().Where(scope.predicate(), deliverylog.CreatedAtGTE(since))

	// One grouped count per (channel, status), using the (tenant_id, created_at) index.
	var byStatus []struct {
		Channel string `json:"channel"`
		Status  string `json:"status"`
		Count   int    `json:"count"`
	}
	if err := base.Clone().GroupBy(deliverylog.FieldChannel, deliverylog.FieldStatus).
		Aggregate(ent.Count()).Scan(ctx, &byStatus); err != nil {
		h.log.Warn("delivery stats query failed", zap.Error(err))
		writeAnalyticsJSON(w, resp)
		return
	}
	accepted := 0
	for _, row := range byStatus {
		switch row.Status {
		case "sent", "delivered":
			accepted += row.Count
			resp.ChannelBreakdown[row.Channel] += row.Count
		case "failed":
			resp.Failed += row.Count
		case "skipped":
			resp.Skipped += row.Count
		}
	}
	resp.TotalSent = accepted
	if attempted := accepted + resp.Failed; attempted > 0 {
		resp.DeliveryRate = float64(accepted) / float64(attempted) * 100
		resp.ErrorRate = float64(resp.Failed) / float64(attempted) * 100
	}

	// Daily series, grouped in SQL and returned oldest first.
	var series []struct {
		Day       string `json:"day"`
		Attempted int    `json:"attempted"`
		Accepted  int    `json:"accepted"`
	}
	err := base.Clone().Modify(func(s *sql.Selector) {
		day := "to_char(date_trunc('day', " + s.C(deliverylog.FieldCreatedAt) + "), 'YYYY-MM-DD')"
		status := s.C(deliverylog.FieldStatus)
		s.Select(
			sql.As(day, "day"),
			sql.As("count(*) FILTER (WHERE "+status+" <> 'skipped')", "attempted"),
			sql.As("count(*) FILTER (WHERE "+status+" IN ('sent','delivered'))", "accepted"),
		).GroupBy(day).OrderBy(day)
	}).Scan(ctx, &series)
	if err != nil {
		h.log.Warn("delivery series query failed", zap.Error(err))
	}
	for _, p := range series {
		resp.TimeSeries = append(resp.TimeSeries, TimeSeriesPoint{Date: p.Day, Sent: p.Attempted, Delivered: p.Accepted})
	}
	sort.SliceStable(resp.TimeSeries, func(i, j int) bool { return resp.TimeSeries[i].Date < resp.TimeSeries[j].Date })

	writeAnalyticsJSON(w, resp)
}

// ActivityLogEntry matches the notifications-ui ActivityLog type. Recipient is always masked:
// lists never show a full address, phone number or device token.
type ActivityLogEntry struct {
	ID           string `json:"id"`
	TemplateName string `json:"templateName"`
	Channel      string `json:"channel"`
	Recipient    string `json:"recipient"`
	Status       string `json:"status"`
	Timestamp    string `json:"timestamp"`
}

// ActivityLogsResponse wraps a page of log entries with the total matching count, so the UI's
// DataTable can render a real page count instead of guessing from a capped-at-100 page size.
type ActivityLogsResponse struct {
	Logs  []ActivityLogEntry `json:"logs"`
	Total int                `json:"total"`
}

// Logs returns delivery log entries for the UI monitoring page.
//
// @Summary      Delivery log
// @Description  Returns paginated delivery log entries with masked recipients. Query: limit, offset, channel, status, from, to, scope (all = platform-wide, platform owners only).
// @Tags         Analytics
// @Param        tenantId  path      string  false  "Tenant identifier"
// @Param        limit     query     int     false  "Max results (default 20)"
// @Param        offset    query     int     false  "Offset for pagination"
// @Param        channel   query     string  false  "Filter by channel"
// @Param        status    query     string  false  "Filter by status"
// @Success      200       {object}  ActivityLogsResponse
// @Router       /analytics/logs [get]
// @Router       /analytics/logs/{tenantId} [get]
// @Security     bearerAuth
// @Security     ApiKeyAuth
func (h *AnalyticsHandler) Logs(w http.ResponseWriter, r *http.Request) {
	scope := h.resolveScope(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	resp := ActivityLogsResponse{Logs: []ActivityLogEntry{}}
	if h.client == nil {
		writeAnalyticsJSON(w, resp)
		return
	}

	ctx := r.Context()
	baseQ := h.client.DeliveryLog.Query().Where(scope.predicate())
	if channel := r.URL.Query().Get("channel"); channel != "" {
		baseQ = baseQ.Where(deliverylog.Channel(channel))
	}
	if status := r.URL.Query().Get("status"); status != "" {
		baseQ = baseQ.Where(deliverylog.Status(status))
	}
	if from := r.URL.Query().Get("from"); from != "" {
		if t, err := time.Parse(time.RFC3339, from); err == nil {
			baseQ = baseQ.Where(deliverylog.CreatedAtGTE(t))
		}
	}
	if to := r.URL.Query().Get("to"); to != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			baseQ = baseQ.Where(deliverylog.CreatedAtLTE(t))
		}
	}

	total, err := baseQ.Clone().Count(ctx)
	if err != nil {
		h.log.Warn("delivery logs count failed", zap.Error(err))
	}
	resp.Total = total

	logs, err := baseQ.Clone().
		Order(ent.Desc(deliverylog.FieldCreatedAt)).
		Offset(offset).
		Limit(limit).
		All(ctx)
	if err != nil {
		h.log.Warn("delivery logs query failed", zap.Error(err))
		writeAnalyticsJSON(w, resp)
		return
	}

	for _, l := range logs {
		resp.Logs = append(resp.Logs, ActivityLogEntry{
			ID:           l.ID.String(),
			TemplateName: l.TemplateID,
			Channel:      l.Channel,
			Recipient:    displayRecipient(l.Channel, l.Recipient),
			Status:       l.Status,
			Timestamp:    l.CreatedAt.Format(time.RFC3339),
		})
	}
	writeAnalyticsJSON(w, resp)
}

// displayRecipient is what a list shows for a logged recipient. Push rows hold a device summary
// ("2 devices"); older push rows held raw token fragments, which are never shown.
func displayRecipient(channel, recipient string) string {
	if channel == "push" {
		if strings.HasSuffix(recipient, "device") || strings.HasSuffix(recipient, "devices") {
			return recipient
		}
		return "device"
	}
	return pii.Mask(recipient)
}

func writeAnalyticsJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
