// Package whatsappinbox persists inbound WhatsApp messages (previously only logged, never stored —
// see webhooks.go's WhatsAppIncoming) and lets tenant staff reply, so a Meta Cloud API-registered
// number — which can't be used from the regular WhatsApp app once registered — actually has a
// usable inbox instead of vanishing into a log line.
package whatsappinbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/devicetoken"
	"github.com/bengobox/notifications-api/internal/ent/whatsappconversation"
	"github.com/bengobox/notifications-api/internal/ent/whatsappmessage"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/providers"
	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// ReplyWindow is Meta's own rule, not ours: a business can only send free-form text within 24h of
// the customer's last inbound message; outside it Meta rejects the send outright.
const ReplyWindow = 24 * time.Hour

// ErrWindowClosed is returned by Reply when the 24h customer-service window has closed — checked
// server-side before ever calling Meta, since Meta would reject it anyway.
var ErrWindowClosed = errors.New("the 24h customer-service window has closed for this conversation")

// ErrNotFound is returned when a conversation doesn't exist or doesn't belong to the caller's tenant.
var ErrNotFound = errors.New("conversation not found")

type Service struct {
	client      *ent.Client
	providerMgr *providers.Manager
	hub         *Hub
	nc          *nats.Conn
	eventsCfg   config.EventsConfig
	log         *zap.Logger
}

func NewService(client *ent.Client, providerMgr *providers.Manager, hub *Hub, nc *nats.Conn, eventsCfg config.EventsConfig, log *zap.Logger) *Service {
	return &Service{client: client, providerMgr: providerMgr, hub: hub, nc: nc, eventsCfg: eventsCfg, log: log.Named("whatsapp-inbox")}
}

// notifyStaff fans a push notification out to every active device token for the tenant (tenant-
// wide, matching the inbox's own RBAC scoping — no per-assignee targeting). Best-effort: a
// tenant with no registered devices, or push not configured, just means no push goes out; the
// message is already persisted and visible in the inbox regardless.
func (s *Service) notifyStaff(ctx context.Context, tenantID uuid.UUID, customerName, customerWaID, preview string) {
	if s.nc == nil {
		return
	}
	tokens, err := s.client.DeviceToken.Query().
		Where(devicetoken.TenantID(tenantID), devicetoken.IsActive(true)).
		All(ctx)
	if err != nil {
		s.log.Warn("whatsapp inbox: device token lookup failed", zap.Error(err))
		return
	}
	if len(tokens) == 0 {
		return
	}
	toks := make([]string, 0, len(tokens))
	for _, t := range tokens {
		toks = append(toks, t.Token)
	}
	msg := messaging.Message{
		TenantID:    tenantID.String(),
		Channel:     "push",
		TemplateID:  "whatsapp/new_message",
		SenderScope: messaging.SenderScopeTenant,
		Target:      messaging.TargetStaff,
		To:          messaging.NormalizeRecipients(toks, "push"),
		Data: map[string]any{
			"customer_name": customerName,
			"customer_wa_id": customerWaID,
			"preview":       preview,
		},
		Metadata:  map[string]any{"push_title": "New WhatsApp message"},
		RequestID: uuid.New().String(),
		QueuedAt:  time.Now(),
	}
	if _, err := messaging.Publish(ctx, s.nc, s.eventsCfg, msg); err != nil {
		s.log.Warn("whatsapp inbox: push publish failed", zap.Error(err))
	}
}

// broadcast is nil-safe — the hub is optional so this package still works if it's never wired.
func (s *Service) broadcast(tenantID uuid.UUID, conversationID uuid.UUID) {
	if s.hub == nil {
		return
	}
	s.hub.BroadcastToTenant(tenantID, StreamMessage{
		Type:    "whatsapp_message",
		Payload: map[string]any{"conversation_id": conversationID.String()},
	})
}

// RecordInbound persists one inbound customer message, idempotent on waMessageID — Meta redelivers
// webhooks, this must never double-insert. Find-or-creates the owning conversation and advances
// both last_message_at and last_inbound_at (the latter is what the reply-window check reads).
func (s *Service) RecordInbound(ctx context.Context, tenantID uuid.UUID, phoneNumberID, waMessageID, fromWaID, msgType, body, contactName string) error {
	if waMessageID != "" {
		exists, err := s.client.WhatsAppMessage.Query().
			Where(whatsappmessage.WaMessageID(waMessageID)).
			Exist(ctx)
		if err != nil {
			return fmt.Errorf("check existing message: %w", err)
		}
		if exists {
			return nil // already recorded — redelivered webhook, not an error
		}
	}
	if body == "" {
		body = fmt.Sprintf("[unsupported message type: %s]", msgType)
	}

	conv, err := s.findOrCreateConversation(ctx, tenantID, phoneNumberID, fromWaID, contactName)
	if err != nil {
		return fmt.Errorf("find or create conversation: %w", err)
	}

	now := time.Now()
	preview := body
	if len(preview) > 140 {
		preview = preview[:140]
	}

	create := s.client.WhatsAppMessage.Create().
		SetConversationID(conv.ID).
		SetTenantID(tenantID).
		SetDirection(whatsappmessage.DirectionInbound).
		SetMessageType(msgType).
		SetBody(body)
	if waMessageID != "" {
		create = create.SetWaMessageID(waMessageID)
	}
	if _, err := create.Save(ctx); err != nil {
		return fmt.Errorf("save inbound message: %w", err)
	}

	update := conv.Update().
		SetLastMessageAt(now).
		SetLastInboundAt(now).
		SetLastMessagePreview(preview).
		AddUnreadCount(1)
	if contactName != "" && conv.CustomerName != contactName {
		update = update.SetCustomerName(contactName)
	}
	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("update conversation: %w", err)
	}
	s.broadcast(tenantID, conv.ID)
	s.notifyStaff(ctx, tenantID, contactName, fromWaID, preview)
	return nil
}

// RecordStatusUpdate updates an outbound message's delivery status from Meta's status webhook.
// Best-effort: a status event for a message sent through the template/notification pipeline
// (not through this inbox) simply won't be found, which is expected, not an error.
func (s *Service) RecordStatusUpdate(ctx context.Context, waMessageID, status string) error {
	if waMessageID == "" {
		return nil
	}
	msg, err := s.client.WhatsAppMessage.Query().
		Where(whatsappmessage.WaMessageID(waMessageID)).
		Only(ctx)
	if err != nil {
		return nil // not found (or not ours) — no-op, not an error
	}
	_, err = msg.Update().SetStatus(status).Save(ctx)
	return err
}

func (s *Service) findOrCreateConversation(ctx context.Context, tenantID uuid.UUID, phoneNumberID, customerWaID, contactName string) (*ent.WhatsAppConversation, error) {
	conv, err := s.client.WhatsAppConversation.Query().
		Where(
			whatsappconversation.TenantID(tenantID),
			whatsappconversation.PhoneNumberID(phoneNumberID),
			whatsappconversation.CustomerWaID(customerWaID),
		).
		Only(ctx)
	if err == nil {
		return conv, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	create := s.client.WhatsAppConversation.Create().
		SetTenantID(tenantID).
		SetPhoneNumberID(phoneNumberID).
		SetCustomerWaID(customerWaID)
	if contactName != "" {
		create = create.SetCustomerName(contactName)
	}
	return create.Save(ctx)
}

// ListConversations returns a tenant's conversations, most recently active first.
func (s *Service) ListConversations(ctx context.Context, tenantID uuid.UUID, p pagination.Params) ([]*ent.WhatsAppConversation, int, error) {
	q := s.client.WhatsAppConversation.Query().
		Where(whatsappconversation.TenantID(tenantID)).
		Order(ent.Desc(whatsappconversation.FieldLastMessageAt))

	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	data, err := q.Offset(p.Offset).Limit(p.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	return data, total, nil
}

// ListMessages returns one conversation's messages, oldest first (thread reading order), scoped to
// tenantID — a conversation belonging to another tenant returns ErrNotFound, never its content.
func (s *Service) ListMessages(ctx context.Context, tenantID, conversationID uuid.UUID, p pagination.Params) ([]*ent.WhatsAppMessage, int, error) {
	if _, err := s.getOwnedConversation(ctx, tenantID, conversationID); err != nil {
		return nil, 0, err
	}
	q := s.client.WhatsAppMessage.Query().
		Where(whatsappmessage.ConversationID(conversationID)).
		Order(ent.Asc(whatsappmessage.FieldCreatedAt))

	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	data, err := q.Offset(p.Offset).Limit(p.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	return data, total, nil
}

// MarkRead zeroes a conversation's unread counter.
func (s *Service) MarkRead(ctx context.Context, tenantID, conversationID uuid.UUID) error {
	conv, err := s.getOwnedConversation(ctx, tenantID, conversationID)
	if err != nil {
		return err
	}
	_, err = conv.Update().SetUnreadCount(0).Save(ctx)
	return err
}

// Reply sends a free-form staff reply through the tenant's own configured WhatsApp number and
// persists it. Enforces Meta's 24h window server-side before ever calling Meta — a UI-only check
// is not enough, since the API must remain correct for any other caller too.
func (s *Service) Reply(ctx context.Context, tenantID, conversationID, senderUserID uuid.UUID, body string) (*ent.WhatsAppMessage, error) {
	conv, err := s.getOwnedConversation(ctx, tenantID, conversationID)
	if err != nil {
		return nil, err
	}
	if conv.LastInboundAt == nil || time.Since(*conv.LastInboundAt) >= ReplyWindow {
		return nil, ErrWindowClosed
	}

	prov, err := s.providerMgr.GetWhatsAppProvider(ctx, tenantID.String(), "meta_cloud")
	if err != nil {
		return nil, fmt.Errorf("resolve whatsapp provider: %w", err)
	}

	var waMessageID string
	if mc, ok := prov.(*whatsapp.MetaCloudProvider); ok {
		waMessageID, err = mc.SendTextMessage(ctx, conv.CustomerWaID, body)
	} else {
		err = prov.SendWhatsApp(ctx, "", []string{conv.CustomerWaID}, body, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("send reply: %w", err)
	}

	create := s.client.WhatsAppMessage.Create().
		SetConversationID(conv.ID).
		SetTenantID(tenantID).
		SetDirection(whatsappmessage.DirectionOutbound).
		SetBody(body).
		SetStatus("sent").
		SetSentByUserID(senderUserID)
	if waMessageID != "" {
		create = create.SetWaMessageID(waMessageID)
	}
	msg, err := create.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("save outbound message: %w", err)
	}

	preview := body
	if len(preview) > 140 {
		preview = preview[:140]
	}
	if _, err := conv.Update().SetLastMessageAt(time.Now()).SetLastMessagePreview(preview).Save(ctx); err != nil {
		s.log.Warn("failed to update conversation after reply", zap.Error(err))
	}
	s.broadcast(tenantID, conv.ID)
	return msg, nil
}

func (s *Service) getOwnedConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*ent.WhatsAppConversation, error) {
	conv, err := s.client.WhatsAppConversation.Query().
		Where(
			whatsappconversation.ID(conversationID),
			whatsappconversation.TenantID(tenantID),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return conv, nil
}

// NormalizeWaID strips a leading "+" and any formatting — Meta's webhook payload already sends
// bare digits for `from`, but this guards any caller that passes a human-typed number instead.
func NormalizeWaID(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
