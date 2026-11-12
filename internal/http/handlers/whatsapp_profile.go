package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/whatsapp/profile"
)

// whatsappProfileTimeout bounds one profile call to Meta (a photo is three calls). The calls run on
// a context detached from the request, like Embedded Signup, so a browser that stops waiting never
// leaves a photo uploaded but not set.
const whatsappProfileTimeout = 2 * time.Minute

// maxPhotoRequest caps a photo upload request: the 5 MB photo plus multipart overhead.
const maxPhotoRequest = profile.MaxPhotoBytes + 1<<20

// WhatsAppProfileHandler serves the business profile of the acting tenant's own WhatsApp number:
// the name and photo customers see, and the about text, description, address, email and websites.
type WhatsAppProfileHandler struct {
	svc *profile.Service
	log *zap.Logger
}

// NewWhatsAppProfileHandler creates the handler.
func NewWhatsAppProfileHandler(svc *profile.Service, log *zap.Logger) *WhatsAppProfileHandler {
	return &WhatsAppProfileHandler{svc: svc, log: log.Named("whatsapp-profile")}
}

// RegisterRoutes mounts the profile routes on the tenant /settings router. adminOnly guards all of
// them: they read and write the tenant's number with its own token, which only a tenant admin (the
// role that connects the number) may use.
func (h *WhatsAppProfileHandler) RegisterRoutes(r chi.Router, adminOnly func(http.Handler) http.Handler) {
	if h == nil || h.svc == nil {
		return
	}
	r.With(adminOnly).Get("/whatsapp/profile", h.Get)
	r.With(adminOnly).Put("/whatsapp/profile", h.Update)
	r.With(adminOnly).Post("/whatsapp/profile/photo", h.Photo)
	r.With(adminOnly).Post("/whatsapp/profile/sync-branding", h.SyncBranding)
}

// begin resolves the acting tenant (the token's own; X-Tenant-ID only for a platform owner) and a
// detached context for the calls to Meta.
func (h *WhatsAppProfileHandler) begin(w http.ResponseWriter, r *http.Request) (string, context.Context, context.CancelFunc, bool) {
	tenantID := resolveActingTenantID(r)
	if tenantID == "" {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return "", nil, nil, false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), whatsappProfileTimeout)
	return tenantID, ctx, cancel, true
}

// fail answers a failed call: a missing number or input Meta would refuse is the caller's to fix
// (400); Meta's own refusal comes back as 502 with Meta's reason.
func (h *WhatsAppProfileHandler) fail(w http.ResponseWriter, tenantID, what string, err error) {
	var ge *profile.GraphError
	switch {
	case errors.Is(err, profile.ErrCredentials):
		msg := err.Error()
		if i := strings.Index(msg, ": "); i >= 0 {
			msg = msg[i+2:]
		}
		jsonError(w, http.StatusBadRequest, msg)
	case errors.Is(err, profile.ErrInvalid):
		jsonError(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), profile.ErrInvalid.Error()+": "))
	case errors.As(err, &ge):
		h.log.Warn("whatsapp profile: Meta refused "+what, zap.String("tenant_id", tenantID),
			zap.Int("status", ge.Status), zap.Int("code", ge.Code), zap.String("message", ge.Message), zap.String("fbtrace_id", ge.TraceID))
		jsonResponse(w, http.StatusBadGateway, map[string]any{"error": ge.Error(), "meta": ge})
	default:
		h.log.Error("whatsapp profile: "+what+" failed", zap.String("tenant_id", tenantID), zap.Error(err))
		jsonError(w, http.StatusBadGateway, what+" failed: "+err.Error())
	}
}

// Get returns the number's display name and its review state, quality rating and business profile.
// @Summary WhatsApp business profile of the tenant's own number
// @Tags Settings
// @Produce json
// @Success 200 {object} profile.Profile
// @Failure 400,502 {object} errorResponse
// @Security bearerAuth
// @Router /settings/whatsapp/profile [get]
func (h *WhatsAppProfileHandler) Get(w http.ResponseWriter, r *http.Request) {
	tenantID, ctx, cancel, ok := h.begin(w, r)
	if !ok {
		return
	}
	defer cancel()
	p, err := h.svc.Get(ctx, tenantID)
	if err != nil {
		h.fail(w, tenantID, "reading the profile", err)
		return
	}
	jsonResponse(w, http.StatusOK, p)
}

// Update changes the given profile fields (about, description, address, email, websites, vertical)
// and answers with the profile as Meta now has it.
// @Summary Update the WhatsApp business profile
// @Tags Settings
// @Accept json
// @Produce json
// @Param request body profile.Update true "Fields to change"
// @Success 200 {object} profile.Profile
// @Failure 400,502 {object} errorResponse
// @Security bearerAuth
// @Router /settings/whatsapp/profile [put]
func (h *WhatsAppProfileHandler) Update(w http.ResponseWriter, r *http.Request) {
	tenantID, ctx, cancel, ok := h.begin(w, r)
	if !ok {
		return
	}
	defer cancel()
	var u profile.Update
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&u); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid profile")
		return
	}
	if err := h.svc.Update(ctx, tenantID, u); err != nil {
		h.fail(w, tenantID, "saving the profile", err)
		return
	}
	h.answerProfile(ctx, w, tenantID)
}

type photoURLRequest struct {
	URL string `json:"url"`
}

// Photo sets the profile photo from an uploaded file (multipart field "file") or from a URL
// ({"url": "..."}). JPEG or PNG, at most 5 MB, at least 192x192 (640x640 is best).
// @Summary Set the WhatsApp profile photo
// @Tags Settings
// @Accept multipart/form-data,json
// @Produce json
// @Success 200 {object} profile.Profile
// @Failure 400,502 {object} errorResponse
// @Security bearerAuth
// @Router /settings/whatsapp/profile/photo [post]
func (h *WhatsAppProfileHandler) Photo(w http.ResponseWriter, r *http.Request) {
	tenantID, ctx, cancel, ok := h.begin(w, r)
	if !ok {
		return
	}
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoRequest)

	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		data, rerr := readPhotoFile(r)
		if rerr != nil {
			jsonError(w, http.StatusBadRequest, rerr.Error())
			return
		}
		err = h.svc.SetPhotoBytes(ctx, tenantID, data)
	} else {
		var req photoURLRequest
		if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil || strings.TrimSpace(req.URL) == "" {
			jsonError(w, http.StatusBadRequest, "send the photo as a file, or its address as {\"url\": \"...\"}")
			return
		}
		err = h.svc.SetPhotoFromURL(ctx, tenantID, req.URL)
	}
	if err != nil {
		h.fail(w, tenantID, "setting the photo", err)
		return
	}
	h.log.Info("whatsapp profile photo set", zap.String("tenant_id", tenantID))
	h.answerProfile(ctx, w, tenantID)
}

func readPhotoFile(r *http.Request) ([]byte, error) {
	if err := r.ParseMultipartForm(maxPhotoRequest); err != nil {
		return nil, errors.New("the photo is too large or the upload is broken (at most 5 MB)")
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		if f, _, err = r.FormFile("photo"); err != nil {
			return nil, errors.New("add the photo as the \"file\" field")
		}
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, profile.MaxPhotoBytes+1))
}

type syncBrandingResponse struct {
	profile.SyncReport
	Profile *profile.Profile `json:"profile,omitempty"`
}

// SyncBranding fills the empty profile fields, and the photo when none is set, from the business's
// own branding (name, logo, tagline, email, website). Fields already set are kept.
// @Summary Fill the WhatsApp business profile from the business's branding
// @Tags Settings
// @Produce json
// @Success 200 {object} syncBrandingResponse
// @Failure 400,502 {object} errorResponse
// @Security bearerAuth
// @Router /settings/whatsapp/profile/sync-branding [post]
func (h *WhatsAppProfileHandler) SyncBranding(w http.ResponseWriter, r *http.Request) {
	tenantID, ctx, cancel, ok := h.begin(w, r)
	if !ok {
		return
	}
	defer cancel()
	rep, err := h.svc.SyncFromBranding(ctx, tenantID)
	if err != nil {
		h.fail(w, tenantID, "filling the profile from branding", err)
		return
	}
	h.log.Info("whatsapp profile filled from branding", zap.String("tenant_id", tenantID),
		zap.Strings("filled", rep.Filled), zap.Strings("warnings", rep.Warnings))
	out := syncBrandingResponse{SyncReport: rep}
	if p, gerr := h.svc.Get(ctx, tenantID); gerr == nil {
		out.Profile = &p
	}
	jsonResponse(w, http.StatusOK, out)
}

// answerProfile answers with the profile after a change; when re-reading fails the change still
// went through, so the answer is a plain success.
func (h *WhatsAppProfileHandler) answerProfile(ctx context.Context, w http.ResponseWriter, tenantID string) {
	p, err := h.svc.Get(ctx, tenantID)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]any{"message": "saved"})
		return
	}
	jsonResponse(w, http.StatusOK, p)
}
