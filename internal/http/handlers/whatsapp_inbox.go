package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	httpware "github.com/Bengo-Hub/httpware"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/modules/whatsappinbox"
)

// WhatsAppInboxHandler exposes the tenant-scoped WhatsApp conversation inbox — list conversations,
// read a thread, mark read, and reply. All routes read the tenant from the JWT (never a client-
// supplied value) via httpware.GetTenantID, same convention as every other tenant-scoped route.
type WhatsAppInboxHandler struct {
	service *whatsappinbox.Service
	log     *zap.Logger
}

func NewWhatsAppInboxHandler(service *whatsappinbox.Service, log *zap.Logger) *WhatsAppInboxHandler {
	return &WhatsAppInboxHandler{service: service, log: log.Named("whatsapp-inbox-handler")}
}

type conversationResponse struct {
	ID                  string  `json:"id"`
	CustomerWaID        string  `json:"customer_wa_id"`
	CustomerName        string  `json:"customer_name,omitempty"`
	LastMessageAt       string  `json:"last_message_at"`
	LastMessagePreview  string  `json:"last_message_preview,omitempty"`
	UnreadCount         int     `json:"unread_count"`
	WindowOpen          bool    `json:"window_open"`
	WindowExpiresAt     *string `json:"window_expires_at,omitempty"`
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
	userID, uerr := uuid.Parse(httpware.GetUserID(r.Context()))
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


