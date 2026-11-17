package whatsappinbox

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
	enttenant "github.com/bengobox/notifications-api/internal/ent/tenant"
	"github.com/bengobox/notifications-api/internal/ent/whatsappmessage"
	"github.com/bengobox/notifications-api/internal/ticketing"
)

// TicketCreator opens (or finds) the support ticket for a WhatsApp message in ticketing-api
// (ticketing.Client).
type TicketCreator interface {
	CreateFromWhatsApp(ctx context.Context, tenantID uuid.UUID, in ticketing.CreateRequest) (*ticketing.Ticket, error)
}

// ErrNotConvertible is returned when the chosen message cannot become a ticket: a staff reply,
// a conversation with no customer message yet, or a message stored without its WhatsApp id.
var ErrNotConvertible = errors.New("only a customer's WhatsApp message can become a ticket")

// maxTicketSubject bounds the subject taken from the first line of the message.
const maxTicketSubject = 120

// WithTickets lets staff turn an inbound message into a support ticket. Without it the convert
// action answers ticketing.ErrDisabled.
func (s *Service) WithTickets(c TicketCreator) *Service {
	s.tickets = c
	return s
}

// ConvertToTicket opens the support ticket for one customer message of the conversation, or
// returns the ticket it already has. A nil messageID takes the conversation's latest customer
// message. ticketing-api keys the ticket on the WhatsApp message id, the key its own WhatsApp
// ingest uses, so converting twice, or converting a message the ingest already ticketed or
// threaded, answers with the existing ticket.
func (s *Service) ConvertToTicket(ctx context.Context, tenantID, conversationID uuid.UUID, messageID uuid.UUID) (*ticketing.Ticket, error) {
	if s.tickets == nil {
		return nil, ticketing.ErrDisabled
	}
	conv, err := s.getOwnedConversation(ctx, tenantID, conversationID)
	if err != nil {
		return nil, err
	}
	msg, err := s.ticketMessage(ctx, tenantID, conv.ID, messageID)
	if err != nil {
		return nil, err
	}
	return s.tickets.CreateFromWhatsApp(ctx, tenantID, ticketRequest(conv, msg))
}

// ticketMessage loads the message to convert, scoped to the conversation and tenant.
func (s *Service) ticketMessage(ctx context.Context, tenantID, conversationID, messageID uuid.UUID) (*ent.WhatsAppMessage, error) {
	q := s.client.WhatsAppMessage.Query().Where(
		whatsappmessage.ConversationID(conversationID),
		whatsappmessage.TenantID(tenantID),
	)
	var (
		msg *ent.WhatsAppMessage
		err error
	)
	if messageID != uuid.Nil {
		msg, err = q.Where(whatsappmessage.ID(messageID)).Only(ctx)
	} else {
		msg, err = q.Where(whatsappmessage.DirectionEQ(whatsappmessage.DirectionInbound)).
			Order(ent.Desc(whatsappmessage.FieldCreatedAt)).
			First(ctx)
	}
	if err != nil {
		if ent.IsNotFound(err) {
			if messageID != uuid.Nil {
				return nil, ErrNotFound
			}
			return nil, ErrNotConvertible
		}
		return nil, err
	}
	if msg.Direction != whatsappmessage.DirectionInbound || strings.TrimSpace(msg.WaMessageID) == "" {
		return nil, ErrNotConvertible
	}
	return msg, nil
}

// TenantSlug is the tenant's slug from the local tenant copy, or "" when it is not known here.
func (s *Service) TenantSlug(ctx context.Context, tenantID uuid.UUID) string {
	t, err := s.client.Tenant.Query().Where(enttenant.ID(tenantID)).Only(ctx)
	if err != nil {
		return ""
	}
	return t.Slug
}

// ticketRequest builds the ticket for a message: the subject is its first line, the description
// the whole message, and the requester the conversation's customer.
func ticketRequest(conv *ent.WhatsAppConversation, msg *ent.WhatsAppMessage) ticketing.CreateRequest {
	body := strings.TrimSpace(msg.Body)
	name := strings.TrimSpace(conv.CustomerName)
	phone := NormalizeWaID(conv.CustomerWaID)
	if phone != "" {
		phone = "+" + phone
	}
	subject := ticketSubject(body)
	if subject == "" {
		who := name
		if who == "" {
			who = phone
		}
		subject = strings.TrimSpace("WhatsApp message from " + who)
	}
	if body == "" {
		body = subject
	}
	return ticketing.CreateRequest{
		Subject:        subject,
		Description:    body,
		RequesterName:  name,
		RequesterPhone: phone,
		WAMessageID:    strings.TrimSpace(msg.WaMessageID),
		PhoneNumberID:  conv.PhoneNumberID,
		ReceivedAt:     msg.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// ticketSubject is the first non-blank line of the message, spaces collapsed, at most
// maxTicketSubject characters.
func ticketSubject(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if line = strings.Join(strings.Fields(line), " "); line != "" {
			return truncateRunes(line, maxTicketSubject)
		}
	}
	return ""
}
