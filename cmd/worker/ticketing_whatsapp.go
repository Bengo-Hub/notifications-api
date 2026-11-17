package main

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// ticketWhatsAppTemplate is the text body (templates/whatsapp/ticketing/ticket_reply.txt) of an
// agent reply sent as free-form WhatsApp text.
const ticketWhatsAppTemplate = "ticketing/ticket_reply"

// Why an agent reply on a WhatsApp ticket is not sent on WhatsApp.
const (
	ticketWhatsAppNotWhatsApp = "not a WhatsApp ticket"
	ticketWhatsAppNoPhone     = "no requester phone"
	ticketWhatsAppNoText      = "empty reply"
	// No approved Meta template carries a support agent's reply (the manifest has none for
	// ticketing, and broadcast_service_notice_v1 is a MARKETING broadcast notice), so a reply after
	// the customer's 24-hour window goes by email only until one is approved.
	ticketWhatsAppWindowClosed = "the 24-hour WhatsApp window is closed and no approved template carries a ticket reply"
)

// ticketWhatsAppReply builds the WhatsApp copy of a ticket.comment_added: the agent's reply as
// free-form text from the tenant's own number, while the customer's 24-hour window is open.
// skip says why nothing is sent; the email copy goes as before either way.
func ticketWhatsAppReply(evt ticketingEvent, ti *tenantInfo, now time.Time) (msg messaging.Message, skip string) {
	p := evt.Payload
	if ticketText(p, "channel") != "whatsapp" {
		return messaging.Message{}, ticketWhatsAppNotWhatsApp
	}
	if at := ticketText(p, "author_type"); at != "" && at != "agent" {
		return messaging.Message{}, ticketWhatsAppNotWhatsApp
	}
	phone := ticketText(p, "requester_phone")
	if phone == "" {
		return messaging.Message{}, ticketWhatsAppNoPhone
	}
	text := ticketText(p, "whatsapp_text")
	if text == "" {
		text = ticketText(p, "comment")
	}
	if text == "" {
		return messaging.Message{}, ticketWhatsAppNoText
	}
	if !ticketWhatsAppWindowOpen(p, now) {
		return messaging.Message{}, ticketWhatsAppWindowClosed
	}
	// The worker sends from the tenant's own registered number (providers.Manager), the number the
	// customer wrote to: a tenant has one WhatsApp number.
	md := map[string]any{"service_id": "ticketing"}
	if ti != nil {
		if code := dialCodeForCountry(ti.Country); code != "" {
			md["default_dial_code"] = code
		}
	}
	return messaging.Message{
		TenantID:    evt.TenantID,
		Channel:     "whatsapp",
		TemplateID:  ticketWhatsAppTemplate,
		SenderScope: messaging.SenderScopeTenant,
		Target:      messaging.TargetCustomer,
		To:          []string{phone},
		// No link in the data: the link policy would turn it into a button, and an interactive
		// message body is limited to 1024 characters, which a long reply exceeds.
		Data: map[string]any{
			"comment":   text,
			"ticket_id": ticketText(p, "ticket_number"),
		},
		Metadata:       md,
		RequestID:      uuid.New().String(),
		IdempotencyKey: ticketIdempotencyKey(evt),
		QueuedAt:       now,
	}, ""
}

// ticketWhatsAppWindowOpen trusts ticketing's whatsapp_window_open and checks it again against
// whatsapp_window_expires_at, so an event that waited in the queue past the window is not sent as
// free-form text Meta would refuse.
func ticketWhatsAppWindowOpen(p map[string]any, now time.Time) bool {
	if open, _ := p["whatsapp_window_open"].(bool); !open {
		return false
	}
	exp := ticketText(p, "whatsapp_window_expires_at")
	if exp == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return true
	}
	return now.Before(t)
}

// dispatchTicketWhatsApp queues the WhatsApp copy of an agent reply on a WhatsApp ticket, or logs
// why there is none. It returns the publish error so the event is retried.
func dispatchTicketWhatsApp(ctx context.Context, nc *nats.Conn, cfg *config.Config, evt ticketingEvent, ti *tenantInfo, logg *zap.Logger) error {
	msg, skip := ticketWhatsAppReply(evt, ti, time.Now())
	switch skip {
	case "":
	case ticketWhatsAppNotWhatsApp:
		return nil
	default:
		logg.Info("ticket reply not sent on whatsapp", zap.String("tenant_id", evt.TenantID),
			zap.String("ticket_number", ticketText(evt.Payload, "ticket_number")), zap.String("reason", skip))
		return nil
	}
	if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
		return err
	}
	logg.Info("ticket reply dispatched on whatsapp", zap.String("tenant_id", evt.TenantID),
		zap.String("ticket_number", ticketText(evt.Payload, "ticket_number")))
	return nil
}
