package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/modules/announcements"
)

// AnnouncementHandler serves the platform "what's new" banners.
type AnnouncementHandler struct {
	svc *announcements.Service
	log *zap.Logger
}

// NewAnnouncementHandler builds the handler.
func NewAnnouncementHandler(svc *announcements.Service, log *zap.Logger) *AnnouncementHandler {
	return &AnnouncementHandler{svc: svc, log: log}
}

// announcementView is what an app's banner receives: content and schedule, never who wrote it.
type announcementView struct {
	ID          uuid.UUID  `json:"id"`
	Title       string     `json:"title"`
	Summary     string     `json:"summary"`
	Highlights  []string   `json:"highlights"`
	CTALabel    string     `json:"cta_label,omitempty"`
	CTAURL      string     `json:"cta_url,omitempty"`
	Audience    string     `json:"audience"`
	Tone        string     `json:"tone"`
	Priority    int        `json:"priority"`
	Dismissible bool       `json:"dismissible"`
	StartsAt    time.Time  `json:"starts_at"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func viewOf(a *ent.Announcement) announcementView {
	h := a.Highlights
	if h == nil {
		h = []string{}
	}
	return announcementView{
		ID: a.ID, Title: a.Title, Summary: a.Summary, Highlights: h, CTALabel: a.CtaLabel, CTAURL: a.CtaURL,
		Audience: string(a.Audience), Tone: string(a.Tone), Priority: a.Priority, Dismissible: a.Dismissible,
		StartsAt: a.StartsAt, EndsAt: a.EndsAt, UpdatedAt: a.UpdatedAt,
	}
}

// Active godoc
// @Summary Active announcements for an app
// @Description The banners an app's dashboard shows now (public, cached). Dismissal is per user, client side.
// @Tags Announcements
// @Produce json
// @Param service query string true "App key, e.g. pos, treasury, inventory"
// @Success 200 {object} map[string]any "announcements: []"
// @Router /api/v1/announcements/active [get]
func (h *AnnouncementHandler) Active(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Active(r.Context(), r.URL.Query().Get("service"))
	if err != nil {
		h.log.Error("list active announcements", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to load announcements")
		return
	}
	out := make([]announcementView, 0, len(list))
	for _, a := range list {
		out = append(out, viewOf(a))
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	// Public, credential-free data read by every app (pos, inventory, ...), most of which are not
	// in HTTP_ALLOWED_ORIGINS. Allow any origin when the CORS middleware did not already name one.
	if w.Header().Get("Access-Control-Allow-Origin") == "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}
	respondJSON(w, http.StatusOK, map[string]any{"announcements": out})
}

// RegisterPlatformRoutes mounts the platform admin routes (the caller applies the super admin gate).
func (h *AnnouncementHandler) RegisterPlatformRoutes(r chi.Router) {
	r.Get("/announcements", h.List)
	r.Post("/announcements", h.Create)
	r.Put("/announcements/{id}", h.Update)
	r.Delete("/announcements/{id}", h.Delete)
}

// List godoc
// @Summary List announcements (platform)
// @Tags Announcements
// @Produce json
// @Success 200 {object} map[string]any
// @Router /api/v1/platform/announcements [get]
func (h *AnnouncementHandler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		h.log.Error("list announcements", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list announcements")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"announcements": list})
}

// Create godoc
// @Summary Publish an announcement (platform)
// @Tags Announcements
// @Accept json
// @Produce json
// @Param body body announcements.Input true "Announcement"
// @Success 201 {object} map[string]any
// @Router /api/v1/platform/announcements [post]
func (h *AnnouncementHandler) Create(w http.ResponseWriter, r *http.Request) {
	var in announcements.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	createdBy := ""
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
		createdBy = claims.Email
	}
	a, err := h.svc.Create(r.Context(), in, createdBy)
	if h.writeErr(w, err, "create announcement") {
		return
	}
	respondJSON(w, http.StatusCreated, a)
}

// Update godoc
// @Summary Update an announcement (platform)
// @Tags Announcements
// @Accept json
// @Produce json
// @Param id path string true "Announcement ID"
// @Param body body announcements.Input true "Announcement"
// @Success 200 {object} map[string]any
// @Router /api/v1/platform/announcements/{id} [put]
func (h *AnnouncementHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in announcements.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	a, err := h.svc.Update(r.Context(), id, in)
	if h.writeErr(w, err, "update announcement") {
		return
	}
	respondJSON(w, http.StatusOK, a)
}

// Delete godoc
// @Summary Delete an announcement (platform)
// @Tags Announcements
// @Param id path string true "Announcement ID"
// @Success 204
// @Router /api/v1/platform/announcements/{id} [delete]
func (h *AnnouncementHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if h.writeErr(w, h.svc.Delete(r.Context(), id), "delete announcement") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeErr maps a service error to a response; true when it wrote one.
func (h *AnnouncementHandler) writeErr(w http.ResponseWriter, err error, op string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, announcements.ErrInvalid):
		jsonError(w, http.StatusBadRequest, err.Error())
	case ent.IsNotFound(err):
		jsonError(w, http.StatusNotFound, "announcement not found")
	default:
		h.log.Error(op, zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to save announcement")
	}
	return true
}
