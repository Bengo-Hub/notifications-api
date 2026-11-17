package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"nhooyr.io/websocket"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/modules/whatsappinbox"
	"github.com/bengobox/notifications-api/internal/ticketing"
)

// WhatsAppInboxHandler exposes the tenant-scoped WhatsApp conversation inbox — list conversations,
// read a thread, mark read, and reply. All routes read the tenant from the JWT (never a client-
// supplied value) via httpware.GetTenantID, same convention as every other tenant-scoped route.
type WhatsAppInboxHandler struct {
	service        *whatsappinbox.Service
	hub            *whatsappinbox.Hub
	allowedOrigins []string
	log            *zap.Logger

	// usesTicketing reports whether the tenant's plan includes the ticketing service; nil means
	// Convert to ticket is not wired. ticketingConsole is the ticketing console's base URL.
	usesTicketing    func(ctx context.Context, tenantID string) bool
	ticketingConsole string
}

// WithTicketing enables Convert to ticket. usesTicketing answers whether a tenant has the
// ticketing service; consoleURL is where a ticket link points.
func (h *WhatsAppInboxHandler) WithTicketing(usesTicketing func(ctx context.Context, tenantID string) bool, consoleURL string) *WhatsAppInboxHandler {
	h.usesTicketing = usesTicketing
	h.ticketingConsole = consoleURL
	return h
}

func NewWhatsAppInboxHandler(service *whatsappinbox.Service, hub *whatsappinbox.Hub, allowedOrigins []string, log *zap.Logger) *WhatsAppInboxHandler {
	return &WhatsAppInboxHandler{service: service, hub: hub, allowedOrigins: allowedOrigins, log: log.Named("whatsapp-inbox-handler")}
}

type conversationResponse struct {
	ID                 string  `json:"id"`
	CustomerWaID       string  `json:"customer_wa_id"`
	CustomerName       string  `json:"customer_name,omitempty"`
	LastMessageAt      string  `json:"last_message_at"`
	LastMessagePreview string  `json:"last_message_preview,omitempty"`
	UnreadCount        int     `json:"unread_count"`
	WindowOpen         bool    `json:"window_open"`
	WindowExpiresAt    *string `json:"window_expires_at,omitempty"`
}

type messageResponse struct {
	ID           string `json:"id"`
	Direction    string `json:"direction"`
	Body         string `json:"body"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
	SentByUserID string `json:"sent_by_user_id,omitempty"`
}

func toConversationResponse(c *ent.WhatsAppConversation) conversationResponse {
	resp := conversationResponse{
		ID:                 c.ID.String(),
		CustomerWaID:       c.CustomerWaID,
		CustomerName:       c.CustomerName,
		LastMessageAt:      c.LastMessageAt.Format(time.RFC3339),
		LastMessagePreview: c.LastMessagePreview,
		UnreadCount:        c.UnreadCount,
	}
	if c.LastInboundAt != nil {
		expires := c.LastInboundAt.Add(whatsappinbox.ReplyWindow)
		resp.WindowOpen = time.Now().Before(expires)
		s := expires.Format(time.RFC3339)
		resp.WindowExpiresAt = &s
	}
	return resp
}

// ListConversations returns the calling tenant's conversations, most recently active first.
func (h *WhatsAppInboxHandler) ListConversations(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantUUID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}
	p := pagination.Parse(r)
	data, total, err := h.service.ListConversations(r.Context(), tenantID, p)
	if err != nil {
		h.log.Error("list conversations", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list conversations")
		return
	}
	out := make([]conversationResponse, 0, len(data))
	for _, c := range data {
		out = append(out, toConversationResponse(c))
	}
	jsonResponse(w, http.StatusOK, pagination.NewResponse(out, total, p))
}

// ListMessages returns one conversation's thread, oldest first.
func (h *WhatsAppInboxHandler) ListMessages(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantUUID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}
	convID, err := uuid.Parse(chi.URLParam(r, "conversationId"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	p := pagination.Parse(r)
	data, total, err := h.service.ListMessages(r.Context(), tenantID, convID, p)
	if err != nil {
		if errors.Is(err, whatsappinbox.ErrNotFound) {
			jsonError(w, http.StatusNotFound, "conversation not found")
			return
		}
		h.log.Error("list messages", zap.Error(err))
		jsonError(w, http.StatusInternalServerError, "failed to list messages")
		return
	}
	out := make([]messageResponse, 0, len(data))
	for _, m := range data {
		mr := messageResponse{
			ID:        m.ID.String(),
			Direction: string(m.Direction),
			Body:      m.Body,
			Status:    m.Status,
			CreatedAt: m.CreatedAt.Format(time.RFC3339),
		}
		if m.SentByUserID != nil {
			mr.SentByUserID = m.SentByUserID.String()
		}
		out = append(out, mr)
	}
	jsonResponse(w, http.StatusOK, pagination.NewResponse(out, total, p))
}

// MarkRead zeroes a conversation's unread counter.
func (h *WhatsAppInboxHandler) MarkRead(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantUUID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}
	convID, err := uuid.Parse(chi.URLParam(r, "conversationId"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	if err := h.service.MarkRead(r.Context(), tenantID, convID); err != nil {
		if errors.Is(err, whatsappinbox.ErrNotFound) {
			jsonError(w, http.StatusNotFound, "conversation not found")
			return
		}
		jsonError(w, http.StatusInternalServerError, "failed to mark read")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"message": "marked read"})
}

type replyRequest struct {
	Body string `json:"body"`
}

// Reply sends a free-form staff reply. Fails with 409 when Meta's 24h customer-service window has
// closed — checked server-side, not just in the UI.
func (h *WhatsAppInboxHandler) Reply(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantUUID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}
	convID, err := uuid.Parse(chi.URLParam(r, "conversationId"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok {
		jsonError(w, http.StatusUnauthorized, "user identity required")
		return
	}
	userID, uerr := claims.UserID()
	if uerr != nil {
		jsonError(w, http.StatusUnauthorized, "user identity required")
		return
	}

	var req replyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Body == "" {
		jsonError(w, http.StatusBadRequest, "body is required")
		return
	}

	msg, err := h.service.Reply(r.Context(), tenantID, convID, userID, req.Body)
	if err != nil {
		switch {
		case errors.Is(err, whatsappinbox.ErrNotFound):
			jsonError(w, http.StatusNotFound, "conversation not found")
		case errors.Is(err, whatsappinbox.ErrNoCredits):
			jsonResponse(w, http.StatusPaymentRequired, map[string]string{
				"error":   "insufficient_credits",
				"service": "whatsapp_service",
				"message": err.Error(),
			})
		case errors.Is(err, whatsappinbox.ErrWindowClosed):
			jsonResponse(w, http.StatusConflict, map[string]string{
				"error":   "window_closed",
				"message": "The 24h customer-service window has closed. Free-form replies aren't allowed by Meta outside this window.",
			})
		default:
			h.log.Error("send whatsapp reply", zap.Error(err))
			jsonError(w, http.StatusBadGateway, "failed to send reply: "+err.Error())
		}
		return
	}

	jsonResponse(w, http.StatusOK, messageResponse{
		ID:        msg.ID.String(),
		Direction: string(msg.Direction),
		Body:      msg.Body,
		Status:    msg.Status,
		CreatedAt: msg.CreatedAt.Format(time.RFC3339),
	})
}

// StreamInbox upgrades to a WebSocket and pushes a lightweight "whatsapp_message" event whenever
// any conversation in the caller's tenant changes (new inbound message or a staff reply) — the
// client re-fetches the affected conversation/thread rather than receiving the full payload over
// the socket, keeping this hub simple. Tenant-scoped: a connection only ever receives its own
// tenant's events (see whatsappinbox.Hub.BroadcastToTenant).
func (h *WhatsAppInboxHandler) StreamInbox(w http.ResponseWriter, r *http.Request) {
	if h.hub == nil {
		jsonError(w, http.StatusServiceUnavailable, "live updates are not configured")
		return
	}
	tenantID, ok := tenantUUID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: wsOriginPatterns(h.allowedOrigins)})
	if err != nil {
		h.log.Warn("whatsapp inbox: websocket upgrade failed", zap.Error(err))
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	h.hub.ServeWS(r.Context(), conn, tenantID)
}

// PlatformOrigins are the platform's own apps (apex and every subdomain), allowed on top of the
// configured HTTP origins by the router's CORS and by the inbox websocket alike.
var PlatformOrigins = []string{"https://codevertexafrica.com", "https://*.codevertexafrica.com"}

// wsOriginPatterns turns the HTTP allowed origins (full URLs such as
// https://notifications.codevertexafrica.com) into the host patterns the websocket library
// matches the Origin header's host against. Passing the URLs as they are matched nothing, so the
// inbox socket refused the notifications UI (15 November 2026). No configured origins means any.
func wsOriginPatterns(allowed []string) []string {
	if len(allowed) == 0 {
		return []string{"*"}
	}
	out := make([]string, 0, len(allowed)+len(PlatformOrigins))
	for _, o := range append(append([]string(nil), allowed...), PlatformOrigins...) {
		o = strings.TrimSpace(o)
		if i := strings.Index(o, "://"); i >= 0 {
			o = o[i+3:]
		}
		o = strings.TrimRight(o, "/")
		if o != "" {
			out = append(out, o)
		}
	}
	return out
}

// ticketingServiceTag is the ticketing service's tag in a tenant's subscription.
const ticketingServiceTag = "ticketing"

// ticketingAvailable reports whether Convert to ticket is wired and the tenant has the service.
func (h *WhatsAppInboxHandler) ticketingAvailable(r *http.Request, tenantID uuid.UUID) bool {
	return h.usesTicketing != nil && h.usesTicketing(r.Context(), tenantID.String())
}

// TicketingStatus tells the inbox whether to offer Convert to ticket for the calling tenant.
func (h *WhatsAppInboxHandler) TicketingStatus(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantUUID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]bool{"available": h.ticketingAvailable(r, tenantID)})
}

type convertToTicketRequest struct {
	// MessageID is the inbound message to convert; empty takes the latest customer message.
	MessageID string `json:"message_id"`
}

type convertToTicketResponse struct {
	TicketID     string `json:"ticket_id"`
	TicketNumber string `json:"ticket_number"`
	Status       string `json:"status"`
	Created      bool   `json:"created"`
	URL          string `json:"url,omitempty"`
}

// ConvertToTicket opens a support ticket in ticketing-api for one customer message (or the
// conversation's latest one) and answers with its number and console link. Converting the same
// message again answers 200 with the same ticket; a new ticket answers 201.
func (h *WhatsAppInboxHandler) ConvertToTicket(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantUUID(r)
	if !ok {
		jsonError(w, http.StatusBadRequest, "tenant ID required")
		return
	}
	convID, err := uuid.Parse(chi.URLParam(r, "conversationId"))
	if err != nil {
		jsonError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	var req convertToTicketRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	messageID := uuid.Nil
	if s := strings.TrimSpace(req.MessageID); s != "" {
		if messageID, err = uuid.Parse(s); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid message id")
			return
		}
	}
	if !h.ticketingAvailable(r, tenantID) {
		jsonResponse(w, http.StatusForbidden, map[string]string{
			"error":   "ticketing_unavailable",
			"message": "Support tickets are not part of this workspace's plan.",
		})
		return
	}

	ticket, err := h.service.ConvertToTicket(r.Context(), tenantID, convID, messageID)
	if err != nil {
		switch {
		case errors.Is(err, whatsappinbox.ErrNotFound):
			jsonError(w, http.StatusNotFound, "message not found")
		case errors.Is(err, whatsappinbox.ErrNotConvertible):
			jsonError(w, http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, ticketing.ErrDisabled):
			jsonError(w, http.StatusServiceUnavailable, "ticketing is not configured")
		default:
			h.log.Error("convert whatsapp message to ticket", zap.String("tenant_id", tenantID.String()),
				zap.String("conversation_id", convID.String()), zap.Error(err))
			jsonError(w, http.StatusBadGateway, "the ticket could not be opened, try again")
		}
		return
	}
	status := http.StatusOK
	if ticket.Created {
		status = http.StatusCreated
	}
	jsonResponse(w, status, convertToTicketResponse{
		TicketID:     ticket.ID,
		TicketNumber: ticket.TicketNumber,
		Status:       ticket.Status,
		Created:      ticket.Created,
		URL:          ticketing.TicketURL(h.ticketingConsole, h.service.TenantSlug(r.Context(), tenantID), ticket.ID),
	})
}
