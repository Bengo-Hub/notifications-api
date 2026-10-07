package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware/pii"
	authclient "github.com/Bengo-Hub/shared-auth-client"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/modules/broadcasts"
	"github.com/bengobox/notifications-api/internal/modules/occasions"
	"github.com/bengobox/notifications-api/internal/modules/suppression"
)

// BroadcastHandler serves bulk notices, occasion greetings and unsubscribe links. The same
// handlers run under /platform (the platform messaging its tenants) and under the tenant routes
// (a tenant messaging its own customers or staff); the path decides the sender.
type BroadcastHandler struct {
	svc       *broadcasts.Service
	drafter   *broadcasts.Drafter
	occasions *occasions.Service
	suppress  *suppression.Service
	resolvers map[string]broadcasts.Resolver
	client    *ent.Client
	log       *zap.Logger
}

// NewBroadcastHandler builds the handler.
func NewBroadcastHandler(client *ent.Client, svc *broadcasts.Service, drafter *broadcasts.Drafter, occ *occasions.Service,
	supp *suppression.Service, resolvers map[string]broadcasts.Resolver, log *zap.Logger) *BroadcastHandler {
	return &BroadcastHandler{svc: svc, drafter: drafter, occasions: occ, suppress: supp, resolvers: resolvers, client: client, log: log.Named("broadcasts-http")}
}

// RegisterRoutes mounts the sender routes. approve wraps the approval actions with the approve
// permission; the caller applies the manage permission to the whole group.
func (h *BroadcastHandler) RegisterRoutes(r chi.Router, approve func(http.Handler) http.Handler) {
	r.Get("/broadcasts", h.List)
	r.Post("/broadcasts", h.Create)
	r.Get("/broadcasts/summary", h.Summary)
	r.Get("/broadcasts/whatsapp-templates", h.WhatsAppTemplates)
	r.Post("/broadcasts/preview", h.Preview)
	r.Get("/broadcasts/{id}", h.Get)
	r.Put("/broadcasts/{id}", h.Update)
	r.Delete("/broadcasts/{id}", h.Delete)
	r.Get("/broadcasts/{id}/recipients", h.Recipients)
	r.Post("/broadcasts/{id}/estimate", h.Estimate)
	r.Get("/broadcasts/{id}/audience", h.Audience)
	r.Put("/broadcasts/{id}/exclusions", h.Exclusions)
	r.Post("/broadcasts/{id}/{action:submit|pause|resume|cancel}", h.Act)
	r.With(approve).Post("/broadcasts/{id}/{action:approve|reject}", h.Act)

	r.Get("/occasions", h.ListOccasions)
	r.Put("/occasions/{key}", h.SaveOccasion)
	r.Delete("/occasions/{key}", h.DeleteOccasion)
	r.Post("/occasions/{key}/draft", h.DraftOccasion)
}

// s2sBroadcastRequest is a broadcast a sibling service hands over (MarketFlow campaigns). It
// enters the sending tenant's approval queue like one written in notifications-ui.
type s2sBroadcastRequest struct {
	TenantID    string           `json:"tenant_id"`
	RequestedBy string           `json:"requested_by"`
	Source      string           `json:"source"`      // e.g. "marketflow"
	SourceRef   string           `json:"source_ref"`  // e.g. the campaign id
	Submit      bool             `json:"submit"`      // straight to the approval queue
	Broadcast   broadcasts.Input `json:"broadcast"`
}

// S2SCreate godoc
// @Summary Create a tenant broadcast from another service (internal key)
// @Description MarketFlow hands campaign sends to the broadcast engine; they wait for the tenant's approval.
// @Tags Broadcasts
// @Accept json
// @Produce json
// @Success 201 {object} map[string]any
// @Router /api/v1/s2s/broadcasts [post]
func (h *BroadcastHandler) S2SCreate(w http.ResponseWriter, r *http.Request) {
	var req s2sBroadcastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	tid, err := uuid.Parse(req.TenantID)
	if err != nil {
		jsonError(w, http.StatusBadRequest, "tenant_id required")
		return
	}
	by := req.RequestedBy
	if by == "" {
		by = req.Source
	}
	s := broadcasts.Sender{TenantID: &tid, Name: h.drafter.SenderName(r.Context(), &tid), By: by}
	b, err := h.svc.Create(r.Context(), req.Broadcast, s)
	if h.writeErr(w, err, "s2s create broadcast") {
		return
	}
	if req.Source != "" || req.SourceRef != "" {
		meta := b.Metadata
		if meta == nil {
			meta = map[string]any{}
		}
		meta["source"], meta["source_ref"] = req.Source, req.SourceRef
		if saved, err := h.client.Broadcast.UpdateOneID(b.ID).SetMetadata(meta).Save(r.Context()); err == nil {
			b = saved
		}
	}
	if req.Submit {
		if submitted, err := h.svc.Act(r.Context(), b.ID, broadcasts.ActionSubmit, s, ""); err == nil {
			b = submitted
		}
	}
	respondJSON(w, http.StatusCreated, viewBroadcast(b))
}

// RegisterPublicRoutes mounts the unsubscribe endpoints (no login: a link in an email must work).
func (h *BroadcastHandler) RegisterPublicRoutes(r chi.Router) {
	r.Get("/public/unsubscribe/{token}", h.UnsubscribeInfo)
	r.Post("/public/unsubscribe/{token}", h.Unsubscribe)
}

// sender works out who is sending. The platform tenant acting as itself IS the platform: its
// customers are the tenants, so its broadcasts, occasions and approvals are the platform's (one
// list, not a "codevertex as a tenant" copy). Any other acting tenant, including one a platform
// owner picked in the tenant switcher, sends as that tenant to its own customers or staff.
func (h *BroadcastHandler) sender(r *http.Request) (broadcasts.Sender, bool) {
	s := broadcasts.Sender{}
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
		s.By = claims.Email
	}
	id, err := uuid.Parse(resolveActingTenantID(r))
	if err != nil {
		return s, false
	}
	if id.String() == h.drafter.PlatformID {
		s.Platform = true
		s.Name = h.drafter.SenderName(r.Context(), nil)
		return s, true
	}
	s.TenantID = &id
	s.Name = h.drafter.SenderName(r.Context(), &id)
	return s, true
}

func (h *BroadcastHandler) senderOr400(w http.ResponseWriter, r *http.Request) (broadcasts.Sender, bool) {
	s, ok := h.sender(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant required")
	}
	return s, ok
}

// broadcastView is a broadcast as the UI shows it.
type broadcastView struct {
	*ent.Broadcast
	Progress int `json:"progress"` // percent handled (sent, failed, skipped or suppressed)
}

func viewBroadcast(b *ent.Broadcast) broadcastView {
	done := b.SentCount + b.FailedCount + b.SkippedCount + b.SuppressedCount
	p := 0
	if b.TargetCount > 0 {
		p = done * 100 / b.TargetCount
	}
	return broadcastView{Broadcast: b, Progress: p}
}

// List godoc
// @Summary List broadcasts
// @Tags Broadcasts
// @Produce json
// @Param status query string false "Comma separated statuses"
// @Param occasion query bool false "Only occasion greetings"
// @Param limit query int false "Page size (max 100)"
// @Param offset query int false "Offset"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts [get]
func (h *BroadcastHandler) List(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := broadcasts.ListFilter{Occasion: q.Get("occasion") == "true"}
	if st := strings.TrimSpace(q.Get("status")); st != "" {
		f.Status = strings.Split(st, ",")
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.Offset, _ = strconv.Atoi(q.Get("offset"))
	list, total, err := h.svc.List(r.Context(), s.TenantID, f)
	if err != nil {
		h.log.Error("list broadcasts", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list broadcasts")
		return
	}
	out := make([]broadcastView, 0, len(list))
	for _, b := range list {
		out = append(out, viewBroadcast(b))
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": out, "total": total})
}

// Summary godoc
// @Summary Broadcasts awaiting approval (menu badge)
// @Tags Broadcasts
// @Produce json
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/summary [get]
func (h *BroadcastHandler) Summary(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	n, err := h.svc.PendingApprovalCount(r.Context(), s.TenantID)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to count")
		return
	}
	scope := "tenant"
	if s.Platform {
		scope = "platform"
	}
	// scope tells the UI who the audience can be: tenants (platform) or customers and staff.
	respondJSON(w, http.StatusOK, map[string]any{"pending_approval": n, "scope": scope, "sender_name": s.Name})
}

// Create godoc
// @Summary Create a broadcast draft
// @Tags Broadcasts
// @Accept json
// @Produce json
// @Param body body broadcasts.Input true "Broadcast"
// @Success 201 {object} map[string]any
// @Router /api/v1/broadcasts [post]
func (h *BroadcastHandler) Create(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	var in broadcasts.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	b, err := h.svc.Create(r.Context(), in, s)
	if h.writeErr(w, err, "create broadcast") {
		return
	}
	respondJSON(w, http.StatusCreated, viewBroadcast(b))
}

// Get godoc
// @Summary Get a broadcast with its per-channel counts
// @Tags Broadcasts
// @Produce json
// @Param id path string true "Broadcast ID"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/{id} [get]
func (h *BroadcastHandler) Get(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	b, err := h.svc.Get(r.Context(), id, s.TenantID)
	if h.writeErr(w, err, "get broadcast") {
		return
	}
	counts, err := h.svc.ChannelCounts(r.Context(), b.ID)
	if err != nil {
		h.log.Warn("broadcast counts", zap.Error(err))
	}
	respondJSON(w, http.StatusOK, map[string]any{"broadcast": viewBroadcast(b), "channels": counts})
}

// Update godoc
// @Summary Edit a draft or a broadcast awaiting approval
// @Tags Broadcasts
// @Accept json
// @Produce json
// @Param id path string true "Broadcast ID"
// @Param body body broadcasts.Input true "Broadcast"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/{id} [put]
func (h *BroadcastHandler) Update(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in broadcasts.Input
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	b, err := h.svc.Update(r.Context(), id, in, s)
	if h.writeErr(w, err, "update broadcast") {
		return
	}
	respondJSON(w, http.StatusOK, viewBroadcast(b))
}

// Delete godoc
// @Summary Delete a draft
// @Tags Broadcasts
// @Param id path string true "Broadcast ID"
// @Success 204
// @Router /api/v1/broadcasts/{id} [delete]
func (h *BroadcastHandler) Delete(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if h.writeErr(w, h.svc.Delete(r.Context(), id, s.TenantID), "delete broadcast") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Act godoc
// @Summary Submit, approve, reject, pause, resume or cancel a broadcast
// @Tags Broadcasts
// @Accept json
// @Produce json
// @Param id path string true "Broadcast ID"
// @Param action path string true "submit | approve | reject | pause | resume | cancel"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/{id}/{action} [post]
func (h *BroadcastHandler) Act(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	b, err := h.svc.Act(r.Context(), id, chi.URLParam(r, "action"), s, strings.TrimSpace(body.Note))
	if h.writeErr(w, err, "broadcast action") {
		return
	}
	respondJSON(w, http.StatusOK, viewBroadcast(b))
}

// recipientView is one recipient as lists show it: the address masked.
type recipientView struct {
	ID          uuid.UUID  `json:"id"`
	Channel     string     `json:"channel"`
	Address     string     `json:"address"`
	DisplayName string     `json:"display_name"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	Attempts    int        `json:"attempts"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
}

// Recipients godoc
// @Summary A broadcast's recipients (addresses masked)
// @Tags Broadcasts
// @Produce json
// @Param id path string true "Broadcast ID"
// @Param status query string false "Filter by status"
// @Param channel query string false "Filter by channel"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/{id}/recipients [get]
func (h *BroadcastHandler) Recipients(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if _, err := h.svc.Get(r.Context(), id, s.TenantID); h.writeErr(w, err, "get broadcast") {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, total, err := h.svc.RecipientPage(r.Context(), id, q.Get("status"), q.Get("channel"), offset, limit)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to list recipients")
		return
	}
	out := make([]recipientView, 0, len(rows))
	for _, rr := range rows {
		addr := ""
		if rr.Address != nil {
			addr = pii.Mask(*rr.Address)
		}
		out = append(out, recipientView{ID: rr.ID, Channel: rr.Channel, Address: addr, DisplayName: rr.DisplayName,
			Status: string(rr.Status), Error: rr.Error, Attempts: rr.Attempts, SentAt: rr.SentAt})
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": out, "total": total})
}

// Estimate godoc
// @Summary Count who a broadcast would reach on each channel, before approving it
// @Tags Broadcasts
// @Produce json
// @Param id path string true "Broadcast ID"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/{id}/estimate [post]
func (h *BroadcastHandler) Estimate(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	b, err := h.svc.Get(r.Context(), id, s.TenantID)
	if h.writeErr(w, err, "get broadcast") {
		return
	}
	est, err := h.estimate(r.Context(), b)
	if err != nil {
		// 424, not 502: the edge replaces 5xx bodies with its own page, which hid the reason.
		if errors.Is(err, broadcasts.ErrAudienceUnavailable) {
			jsonError(w, http.StatusFailedDependency, err.Error())
			return
		}
		h.log.Warn("broadcast estimate", zap.Error(err))
		jsonError(w, http.StatusFailedDependency, "could not reach the contact list right now; try again in a minute")
		return
	}
	respondJSON(w, http.StatusOK, est)
}

// estimate counts, per channel, who the broadcast would actually send to now: the same review
// as the recipient list (consent, opt-outs and people left out applied), paged and capped so a
// very large audience reports "at least".
func (h *BroadcastHandler) estimate(ctx context.Context, b *ent.Broadcast) (map[string]any, error) {
	res, err := h.resolverFor(b)
	if err != nil {
		return nil, err
	}
	const maxPages = 50
	people, left, cursor := 0, 0, ""
	reach := map[string]int{}
	capped := false
	for page := 0; ; page++ {
		if page == maxPages {
			capped = true
			break
		}
		rows, next, err := broadcasts.Review(ctx, res, h.suppress, b, cursor, 200)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			people++
			if r.Excluded {
				left++
			}
			for ch, rc := range r.Channels {
				if rc.Sends {
					reach[ch]++
				}
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return map[string]any{"people": people, "left_out": left, "reachable": reach, "at_least": capped}, nil
}

func (h *BroadcastHandler) resolverFor(b *ent.Broadcast) (broadcasts.Resolver, error) {
	aType, _ := b.Audience["type"].(string)
	res, ok := h.resolvers[aType]
	if !ok {
		return nil, broadcasts.ErrAudienceUnavailable
	}
	return res, nil
}

// Audience godoc
// @Summary Review who a broadcast goes to (paged), with the address each channel would use
// @Description Addresses are masked. Each row says per channel whether it sends or why not (opted out, no consent, no valid address, left out).
// @Tags Broadcasts
// @Produce json
// @Param id path string true "Broadcast ID"
// @Param after query string false "Cursor from the previous page"
// @Param limit query int false "Page size (max 100)"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/{id}/audience [get]
func (h *BroadcastHandler) Audience(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	b, err := h.svc.Get(r.Context(), id, s.TenantID)
	if h.writeErr(w, err, "get broadcast") {
		return
	}
	res, err := h.resolverFor(b)
	if err != nil {
		jsonError(w, http.StatusFailedDependency, err.Error())
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, next, err := broadcasts.Review(r.Context(), res, h.suppress, b, r.URL.Query().Get("after"), limit)
	if err != nil {
		if !errors.Is(err, broadcasts.ErrAudienceUnavailable) {
			h.log.Warn("broadcast audience review", zap.Error(err))
			err = errors.New("could not reach the contact list right now; try again in a minute")
		}
		jsonError(w, http.StatusFailedDependency, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": rows, "next": next, "left_out": len(stringsOf(b.Metadata["excluded"]))})
}

// Exclusions godoc
// @Summary Leave people out of a broadcast, or put them back, before it sends
// @Tags Broadcasts
// @Accept json
// @Produce json
// @Param id path string true "Broadcast ID"
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/{id}/exclusions [put]
func (h *BroadcastHandler) Exclusions(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var body struct {
		Exclude []string `json:"exclude"`
		Include []string `json:"include"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	n, err := h.svc.SetExclusions(r.Context(), id, s.TenantID, body.Exclude, body.Include)
	if h.writeErr(w, err, "set exclusions") {
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"left_out": n})
}

func stringsOf(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Preview godoc
// @Summary Render a message for a sample recipient and check its placeholders
// @Tags Broadcasts
// @Accept json
// @Produce json
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/preview [post]
func (h *BroadcastHandler) Preview(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	var body struct {
		Texts     map[string]string `json:"texts"`
		FirstName string            `json:"first_name"`
		Business  string            `json:"business_name"`
		Occasion  string            `json:"occasion"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := broadcasts.ValidateText(body.Texts); err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	v := broadcasts.Vars{FirstName: body.FirstName, BusinessName: body.Business, Name: body.Business, SenderName: s.Name, Occasion: body.Occasion}
	if v.FirstName == "" && v.BusinessName == "" {
		v.FirstName = "Titus"
	}
	out := map[string]string{}
	for k, t := range body.Texts {
		out[k] = broadcasts.Render(t, v)
		if k == "sms.body" {
			out["sms.body_with_opt_out"] = broadcasts.WithSMSOptOut(out[k])
		}
	}
	respondJSON(w, http.StatusOK, map[string]any{"rendered": out, "sender_name": s.Name})
}

// WhatsAppTemplates godoc
// @Summary Approved WhatsApp templates a broadcast can use, with the values each one takes
// @Tags Broadcasts
// @Produce json
// @Success 200 {object} map[string]any
// @Router /api/v1/broadcasts/whatsapp-templates [get]
func (h *BroadcastHandler) WhatsAppTemplates(w http.ResponseWriter, r *http.Request) {
	type tpl struct {
		Name   string   `json:"name"`
		Params []string `json:"params"`
	}
	out := make([]tpl, 0, len(broadcasts.OccasionTemplateParams))
	for name, params := range broadcasts.OccasionTemplateParams {
		out = append(out, tpl{Name: name, Params: params})
	}
	respondJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// ListOccasions godoc
// @Summary The sender's occasions with their next date
// @Tags Occasions
// @Produce json
// @Success 200 {object} map[string]any
// @Router /api/v1/occasions [get]
func (h *BroadcastHandler) ListOccasions(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	loc, _ := time.LoadLocation("Africa/Nairobi")
	list, err := h.occasions.List(r.Context(), s.TenantID, time.Now(), loc)
	if err != nil {
		h.log.Error("list occasions", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list occasions")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"data": list})
}

// SaveOccasion godoc
// @Summary Switch an occasion on or off, set auto-send, channels and wording, or add a custom one
// @Tags Occasions
// @Accept json
// @Produce json
// @Param key path string true "Occasion key"
// @Success 200 {object} map[string]any
// @Router /api/v1/occasions/{key} [put]
func (h *BroadcastHandler) SaveOccasion(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	var body struct {
		Name           *string             `json:"name"`
		Rule           map[string]any      `json:"rule"`
		LeadDays       *int                `json:"lead_days"`
		SendOffsetDays *int                `json:"send_offset_days"`
		Settings       *occasions.Settings `json:"settings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Settings != nil {
		texts := map[string]string{}
		for i, v := range body.Settings.Variants {
			texts["wording "+strconv.Itoa(i+1)+" subject"] = v.Subject
			texts["wording "+strconv.Itoa(i+1)+" message"] = v.Body
			texts["wording "+strconv.Itoa(i+1)+" sms"] = v.SMS
		}
		if err := broadcasts.ValidateText(texts); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	key := chi.URLParam(r, "key")
	err := h.occasions.Save(r.Context(), s.TenantID, key, occasions.Update{
		Name: body.Name, Rule: body.Rule, LeadDays: body.LeadDays, SendOffsetDays: body.SendOffsetDays, Settings: body.Settings,
	})
	if err != nil {
		if errors.Is(err, occasions.ErrNotFound) {
			jsonError(w, http.StatusNotFound, err.Error())
			return
		}
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	loc, _ := time.LoadLocation("Africa/Nairobi")
	v, err := h.occasions.Get(r.Context(), s.TenantID, key, time.Now(), loc)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "saved, but could not reload it")
		return
	}
	respondJSON(w, http.StatusOK, v)
}

// DeleteOccasion godoc
// @Summary Remove a tenant's custom occasion or its changes to a shared one
// @Tags Occasions
// @Param key path string true "Occasion key"
// @Success 204
// @Router /api/v1/occasions/{key} [delete]
func (h *BroadcastHandler) DeleteOccasion(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	if s.TenantID == nil {
		jsonError(w, http.StatusBadRequest, "switch shared occasions off instead of deleting them")
		return
	}
	if err := h.occasions.Delete(r.Context(), *s.TenantID, chi.URLParam(r, "key")); err != nil {
		jsonError(w, http.StatusInternalServerError, "failed to delete")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DraftOccasion godoc
// @Summary Create this year's greeting for an occasion now, for review and approval
// @Tags Occasions
// @Produce json
// @Param key path string true "Occasion key"
// @Success 200 {object} map[string]any
// @Router /api/v1/occasions/{key}/draft [post]
func (h *BroadcastHandler) DraftOccasion(w http.ResponseWriter, r *http.Request) {
	s, ok := h.senderOr400(w, r)
	if !ok {
		return
	}
	b, err := h.drafter.DraftNow(r.Context(), s.TenantID, chi.URLParam(r, "key"))
	switch {
	case errors.Is(err, occasions.ErrNotFound):
		jsonError(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, broadcasts.ErrNoOccurrence):
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		h.log.Error("draft occasion", zap.Error(err))
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, viewBroadcast(b))
}

// UnsubscribeInfo godoc
// @Summary What an unsubscribe link is for (public)
// @Tags Unsubscribe
// @Produce json
// @Param token path string true "Signed token from the link"
// @Success 200 {object} map[string]any
// @Router /api/v1/public/unsubscribe/{token} [get]
func (h *BroadcastHandler) UnsubscribeInfo(w http.ResponseWriter, r *http.Request) {
	t, err := h.suppress.Verify(chi.URLParam(r, "token"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "this unsubscribe link is not valid")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		// Opened straight from an email client: a tiny page with a one-click button.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		name := html.EscapeString(firstNonBlank(t.Sender, "this sender"))
		_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Unsubscribe</title></head>` +
			`<body style="font-family:system-ui,sans-serif;max-width:480px;margin:48px auto;padding:0 16px;color:#1f2937">` +
			`<h1 style="font-size:20px">Stop messages from ` + name + `?</h1>` +
			`<p>You will no longer get offers and greetings from ` + name + ` by ` + html.EscapeString(t.Channel) + `. Receipts and account messages still arrive.</p>` +
			`<form method="post"><button style="padding:10px 16px;border-radius:8px;border:1px solid #1f2937;background:#1f2937;color:#fff;font-size:15px">Unsubscribe</button></form>` +
			`</body></html>`))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"sender": t.Sender, "channel": t.Channel})
}

// Unsubscribe godoc
// @Summary Unsubscribe (public, one-click per RFC 8058)
// @Tags Unsubscribe
// @Param token path string true "Signed token from the link"
// @Success 200 {object} map[string]any
// @Router /api/v1/public/unsubscribe/{token} [post]
func (h *BroadcastHandler) Unsubscribe(w http.ResponseWriter, r *http.Request) {
	t, err := h.suppress.Verify(chi.URLParam(r, "token"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "this unsubscribe link is not valid")
		return
	}
	source := "email_link"
	if r.FormValue("List-Unsubscribe") == "One-Click" {
		source = "one_click"
	}
	// The token carries only a hash; the address is still on the recent broadcast row, which lets
	// the contact's owner (MarketFlow) find and update it.
	_, address, _ := broadcasts.LastSender(r.Context(), h.client, t.Channel, t.AddressHash)
	if err := h.suppress.Unsubscribe(r.Context(), t, address, source); err != nil {
		h.log.Error("unsubscribe", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "could not unsubscribe, please try again")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") || r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" && source == "email_link" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Unsubscribed</title></head>` +
			`<body style="font-family:system-ui,sans-serif;max-width:480px;margin:48px auto;padding:0 16px;color:#1f2937">` +
			`<h1 style="font-size:20px">You are unsubscribed</h1><p>You will not get these messages again.</p></body></html>`))
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"unsubscribed": true})
}

func firstNonBlank(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// writeErr maps a service error to a response; true when it wrote one.
func (h *BroadcastHandler) writeErr(w http.ResponseWriter, err error, op string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, broadcasts.ErrNotFound):
		jsonError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, broadcasts.ErrNotEditable), errors.Is(err, broadcasts.ErrBadTransition):
		jsonError(w, http.StatusConflict, err.Error())
	case errors.As(err, new(*broadcasts.ValidationError)), ent.IsValidationError(err):
		jsonError(w, http.StatusBadRequest, err.Error())
	default:
		h.log.Error(op, zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to save broadcast")
	}
	return true
}
