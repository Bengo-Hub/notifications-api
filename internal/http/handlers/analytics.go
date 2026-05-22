package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/deliverylog"
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
type DeliveryStatsResponse struct {
	TotalSent        int               `json:"totalSent"`
	DeliveryRate     float64           `json:"deliveryRate"`
	ErrorRate        float64           `json:"errorRate"`
	ChannelBreakdown map[string]int    `json:"channelBreakdown"`
	TimeSeries       []TimeSeriesPoint `json:"timeSeries"`
}

type TimeSeriesPoint struct {
	Date      string `json:"date"`
	Sent      int    `json:"sent"`
	Delivered int    `json:"delivered"`
}

// Delivery returns delivery stats for a tenant from delivery_log store.
//
// @Summary      Delivery analytics
// @Description  Returns delivery statistics for the tenant. Query: range (e.g. 24h, 7d).
// @Tags         Analytics
// @Param        tenantId  path      string  true   "Tenant identifier"
// @Param        range     query     string  false  "Time range (e.g. 24h, 7d)"
// @Success      200       {object}  DeliveryStatsResponse
// @Router       /analytics/delivery/{tenantId} [get]
// @Security     bearerAuth
// @Security     ApiKeyAuth
func (h *AnalyticsHandler) Delivery(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantId")
	// Fall back to JWT claims or X-Tenant-ID header when no URL param (non-platform users)
	if tenantID == "" {
		tenantID = httpware.GetTenantID(r.Context())
	}
	if tenantID == "" {
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			tenantID = claims.TenantID
		}
	}
	rangeQ := r.URL.Query().Get("range")
	if rangeQ == "" {
		rangeQ = "24h"
	}

	var since time.Time
	switch rangeQ {
	case "7d":
		since = time.Now().Add(-7 * 24 * time.Hour)
	case "24h":
		fallthrough
	default:
		since = time.Now().Add(-24 * time.Hour)
	}

	resp := DeliveryStatsResponse{
		TotalSent:        0,
		DeliveryRate:     0,
		ErrorRate:        0,
		ChannelBreakdown: map[string]int{"email": 0, "sms": 0, "push": 0},
		TimeSeries:       []TimeSeriesPoint{},
	}

	if h.client == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	ctx := r.Context()
	logs, err := h.client.DeliveryLog.Query().
		Where(
			deliverylog.TenantID(tenantID),
			deliverylog.CreatedAtGTE(since),
		).
		All(ctx)
	if err != nil {
		h.log.Warn("delivery stats query failed", zap.Error(err))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	resp.TotalSent = len(logs)
	delivered := 0
	failed := 0
	for _, l := range logs {
		resp.ChannelBreakdown[l.Channel]++
		switch l.Status {
		case "delivered":
			delivered++
		case "failed":
			failed++
		default:
			delivered++ // treat "sent" as delivered for rate when no worker updates
		}
	}
	if resp.TotalSent > 0 {
		resp.DeliveryRate = float64(delivered) / float64(resp.TotalSent) * 100
		resp.ErrorRate = float64(failed) / float64(resp.TotalSent) * 100
	}

	// Time series: group by date
	byDate := make(map[string]*TimeSeriesPoint)
	for _, l := range logs {
		date := l.CreatedAt.Format("2006-01-02")
		if byDate[date] == nil {
			byDate[date] = &TimeSeriesPoint{Date: date}
		}
		byDate[date].Sent++
		if l.Status == "delivered" || l.Status == "sent" {
			byDate[date].Delivered++
		}
	}
	for _, p := range byDate {
		resp.TimeSeries = append(resp.TimeSeries, *p)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ActivityLogEntry matches the notifications-ui ActivityLog type.
type ActivityLogEntry struct {
	ID           string `json:"id"`
	TemplateName string `json:"templateName"`
	Channel      string `json:"channel"`
	Recipient    string `json:"recipient"`
	Status       string `json:"status"`
	Timestamp    string `json:"timestamp"`
}

// Logs returns delivery log entries for the UI monitoring page.
//
// @Summary      Delivery log
// @Description  Returns paginated delivery log entries for the tenant. Query: limit, offset, channel, status, from, to.
// @Tags         Analytics
// @Param        tenantId  path      string  true   "Tenant identifier"
// @Param        limit     query     int     false  "Max results (default 20)"
// @Param        offset    query     int     false  "Offset for pagination"
// @Param        channel   query     string  false  "Filter by channel"
// @Param        status    query     string  false  "Filter by status"
// @Success      200       {array}   ActivityLogEntry
// @Router       /analytics/logs/{tenantId} [get]
// @Security     bearerAuth
// @Security     ApiKeyAuth
func (h *AnalyticsHandler) Logs(w http.ResponseWriter, r *http.Request) {
	tenantID := chi.URLParam(r, "tenantId")
	// Fall back to JWT claims or X-Tenant-ID header when no URL param (non-platform users)
	if tenantID == "" {
		tenantID = httpware.GetTenantID(r.Context())
	}
	if tenantID == "" {
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			tenantID = claims.TenantID
		}
	}
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
	channel := r.URL.Query().Get("channel")
	status := r.URL.Query().Get("status")

	entries := []ActivityLogEntry{}
	if h.client == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
		return
	}

	ctx := r.Context()
	q := h.client.DeliveryLog.Query().
		Where(deliverylog.TenantID(tenantID)).
		Order(ent.Desc(deliverylog.FieldCreatedAt)).
		Offset(offset).
		Limit(limit)
	if channel != "" {
		q = q.Where(deliverylog.Channel(channel))
	}
	if status != "" {
		q = q.Where(deliverylog.Status(status))
	}
	logs, err := q.All(ctx)
	if err != nil {
		h.log.Warn("delivery logs query failed", zap.Error(err))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
		return
	}

	for _, l := range logs {
		entries = append(entries, ActivityLogEntry{
			ID:           l.ID.String(),
			TemplateName: l.TemplateID,
			Channel:      l.Channel,
			Recipient:    l.Recipient,
			Status:       l.Status,
			Timestamp:    l.CreatedAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}
