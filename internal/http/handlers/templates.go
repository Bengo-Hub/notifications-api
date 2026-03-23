package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/notifications-api/internal/modules/templates"
	platformtemplates "github.com/bengobox/notifications-api/internal/platform/templates"
	"github.com/go-chi/chi/v5"
)

// TemplateHandler exposes endpoints for listing and previewing templates.
type TemplateHandler struct {
	loader   *platformtemplates.Loader
	repo     *templates.Repository
	notifier *NotificationHandler
}

func NewTemplateHandler(loader *platformtemplates.Loader, repo *templates.Repository, notifier *NotificationHandler) *TemplateHandler {
	return &TemplateHandler{loader: loader, repo: repo, notifier: notifier}
}

type templateGetResponse struct {
	ID       string `json:"id"`
	Channel  string `json:"channel"`
	Content  string `json:"content"`
	MimeType string `json:"mimeType"`
}

// extractTemplateID gets the template ID from chi's wildcard catch-all.
func extractTemplateID(r *http.Request) string {
	id := chi.URLParam(r, "*")
	id = strings.TrimPrefix(id, "/")
	id = strings.TrimSuffix(id, "/test")
	return id
}

// List returns available notification templates with pagination.
// @Summary List notification templates
// @Description Returns paginated notification templates with optional channel/category/search filters.
// Templates are platform-wide resources accessible by all authenticated users.
// @Tags Templates
// @Produce json
// @Param page query int false "Page number (1-based)"
// @Param limit query int false "Items per page (max 100)"
// @Param channel query string false "Filter by channel (email|sms|push|whatsapp)"
// @Param category query string false "Filter by category (auth|finance|logistics|...)"
// @Param search query string false "Search by name"
// @Success 200 {object} pagination.Response[templates.TemplateSummary]
// @Security bearerAuth
// @Security ApiKeyAuth
// @Router /templates [get]
func (h *TemplateHandler) List(w http.ResponseWriter, r *http.Request) {
	p := pagination.Parse(r)
	f := templates.Filters{
		Channel:  r.URL.Query().Get("channel"),
		Category: r.URL.Query().Get("category"),
		Tag:      r.URL.Query().Get("tag"),
		Search:   r.URL.Query().Get("search"),
	}

	data, total, err := h.repo.List(r.Context(), p, f)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(errorResponse{Error: "failed to list templates"})
		return
	}

	resp := pagination.NewResponse(data, total, p)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Get returns the raw template content for an ID and channel.
// @Summary Get template content
// @Description Returns the raw template content for preview or client-side rendering.
// Templates are platform-wide resources accessible by all authenticated users.
// @Tags Templates
// @Produce json
// @Param id path string true "Template identifier"
// @Param channel query string true "Channel (email|sms|push|whatsapp)"
// @Success 200 {object} templateGetResponse
// @Failure 404 {object} errorResponse
// @Security bearerAuth
// @Security ApiKeyAuth
// @Router /templates/{id} [get]
func (h *TemplateHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := extractTemplateID(r)
	channel := r.URL.Query().Get("channel")
	if id == "" || channel == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "id and channel required"})
		return
	}
	content, err := h.loader.Get(r.Context(), channel+"/"+id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(errorResponse{Error: "template not found"})
		return
	}
	mime := platformtemplates.MimeTypeForExt("." + extensionForChannel(channel))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(templateGetResponse{
		ID:       id,
		Channel:  channel,
		Content:  content,
		MimeType: mime,
	})
}

// extensionForChannel returns the default file extension for a channel.
func extensionForChannel(ch string) string {
	switch ch {
	case "email":
		return "html"
	case "push":
		return "json"
	default:
		return "txt"
	}
}

// templateTestSendRequest is the body for POST .../templates/{id}/test.
type templateTestSendRequest struct {
	Channel string         `json:"channel"` // required if not in query
	To      []string       `json:"to"`      // required
	Data    map[string]any `json:"data"`
}

// TestSend enqueues a test notification for the template (uses existing enqueue path).
// @Summary Send test notification for template
// @Description Sends a test notification using the template to a specific tenant. Message is queued for delivery.
// @Tags Templates
// @Accept json
// @Produce json
// @Param tenantId query string true "Tenant ID where the test notification will be sent"
// @Param id path string true "Template identifier"
// @Param channel query string false "Channel (email|sms|push); can also be in body"
// @Param request body templateTestSendRequest true "Test send payload"
// @Success 202 {object} enqueueResponse
// @Failure 400,404,500 {object} errorResponse
// @Security bearerAuth
// @Security ApiKeyAuth
// @Router /templates/{id}/test [post]
func (h *TemplateHandler) TestSend(w http.ResponseWriter, r *http.Request) {
	// tenantId is required as a query parameter (since templates are platform-wide, not tenant-scoped)
	tenantID := r.URL.Query().Get("tenantId")
	if tenantID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "tenantId query parameter required"})
		return
	}
	id := extractTemplateID(r)
	if id == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "template id required"})
		return
	}
	channel := r.URL.Query().Get("channel")
	var req templateTestSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "invalid request body"})
		return
	}
	if channel == "" {
		channel = req.Channel
	}
	if channel == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "channel required (query or body)"})
		return
	}
	if len(req.To) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "to required (at least one recipient)"})
		return
	}
	// Verify template exists
	if _, err := h.loader.Get(r.Context(), channel+"/"+id); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(errorResponse{Error: "template not found"})
		return
	}
	if h.notifier == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(errorResponse{Error: "test send not configured"})
		return
	}
	data := req.Data
	if data == nil {
		data = map[string]any{"name": "Test User", "org_name": "Test Org"}
	}
	requestID, err := h.notifier.EnqueueMessage(r.Context(), tenantID, channel, id, req.To, data, nil)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(errorResponse{Error: err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(enqueueResponse{Status: "queued", RequestID: requestID})
}

// templateUpdateRequest is the body for PUT/PATCH template.
type templateUpdateRequest struct {
	Content string `json:"content"`
	Subject string `json:"subject"` // optional, for email channel
}

// Update writes template content to the filesystem (platform-wide resource).
// @Summary Update template content
// @Description Updates the template content for the given id and channel. File-based storage.
// Templates are platform-wide; only users with templates:manage permission can update.
// @Tags Templates
// @Accept json
// @Produce json
// @Param id path string true "Template identifier"
// @Param request body templateUpdateRequest true "Template content"
// @Param channel query string true "Channel (email|sms|push|whatsapp)"
// @Success 200 {object} templateGetResponse
// @Failure 400,404 {object} errorResponse
// @Security bearerAuth
// @Security ApiKeyAuth
// @Router /templates/{id} [put]
func (h *TemplateHandler) Update(w http.ResponseWriter, r *http.Request) {
	id := extractTemplateID(r)
	channel := r.URL.Query().Get("channel")
	if id == "" || channel == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "id and channel required"})
		return
	}
	var req templateUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(errorResponse{Error: "invalid request body"})
		return
	}
	if err := h.loader.Write(r.Context(), channel, id, req.Content); err != nil {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(err.Error(), "not under") || strings.Contains(err.Error(), "invalid") {
			w.WriteHeader(http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(errorResponse{Error: err.Error()})
		return
	}

	// Invalidate template cache so next list call picks up changes
	h.repo.InvalidateCache(r.Context())

	// Return updated template
	content, _ := h.loader.Get(r.Context(), channel+"/"+id)
	mime := platformtemplates.MimeTypeForExt("." + extensionForChannel(channel))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(templateGetResponse{
		ID:       id,
		Channel:  channel,
		Content:  content,
		MimeType: mime,
	})
}
