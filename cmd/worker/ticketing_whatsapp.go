package main

import (
	"context"
	"net/url"
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

// After the customer's 24-hour window an agent reply goes as the ticketing_ticket_reply_v1_btn
// template: a notice that the business answered request TKT-n, with a "View reply" button to the
// public ticket page on https://ticketing.codevertexafrica.com/{{1}} (the domain is fixed at
// approval; ticketTrackSuffix is the part after it). The reply text itself is never a template
// variable (a body that is mostly free text is marketing to Meta, docs/whatsapp-template-policy.md
// section 3b).
const (
	ticketReplyMetaTemplate = "ticketing_ticket_reply_v1_btn"
	// ticketReplyNoticeText is the same notice as text (templates/whatsapp/ticketing/
	// ticket_reply_notice.txt), for the delivery log.
	ticketReplyNoticeText = "ticketing/ticket_reply_notice"
)

// Why an agent reply on a WhatsApp ticket is not sent on WhatsApp.
const (
	ticketWhatsAppNotWhatsApp = "not a WhatsApp ticket"
	ticketWhatsAppNoPhone     = "no requester phone"
	ticketWhatsAppNoText      = "empty reply"
	// Outside the window the template needs its button, and the button needs the tenant's slug
	// and the ticket number: a _btn template never goes without its link.
	ticketWhatsAppNoLink = "the 24-hour WhatsApp window is closed and the ticket has no public link for the template button"
)

// ticketWhatsAppOn says whether an event sends a WhatsApp copy at all: agent replies only, and only
// with TICKETING_WHATSAPP_REPLIES on (off by owner decision of 2026-11-17: replies go by email only).
func ticketWhatsAppOn(cfg *config.Config, evt ticketingEvent) bool {
	return evt.EventType == "ticket.comment_added" && cfg != nil && cfg.Services.TicketingWhatsAppReplies
}

// ticketWhatsAppReply builds the WhatsApp copy of a ticket.comment_added, from the tenant's own
// number: inside the customer's 24-hour window the agent's reply as free-form text, after it the
// ticketing_ticket_reply_v1_btn template. skip says why nothing is sent; the email copy goes as
// before either way.
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
	// The worker sends from the tenant's own registered number (providers.Manager), the number the
	// customer wrote to: a tenant has one WhatsApp number.
	md := map[string]any{"service_id": "ticketing"}
	if ti != nil {
		if code := dialCodeForCountry(ti.Country); code != "" {
			md["default_dial_code"] = code
		}
	}
	msg = messaging.Message{
		TenantID:       evt.TenantID,
		Channel:        "whatsapp",
		SenderScope:    messaging.SenderScopeTenant,
		Target:         messaging.TargetCustomer,
		To:             []string{phone},
		Metadata:       md,
		RequestID:      uuid.New().String(),
		IdempotencyKey: ticketIdempotencyKey(evt),
		QueuedAt:       now,
	}
	number := ticketText(p, "ticket_number")

	if ticketWhatsAppWindowOpen(p, now) {
		msg.TemplateID = ticketWhatsAppTemplate
		// No link in the data: the link policy would turn it into a button, and an interactive
		// message body is limited to 1024 characters, which a long reply exceeds.
		msg.Data = map[string]any{"comment": text, "ticket_id": number}
		return msg, ""
	}

	suffix := ticketTrackSuffix(ti, number)
	if suffix == "" {
		return messaging.Message{}, ticketWhatsAppNoLink
	}
	business := ti.Name
	if business == "" {
		business = ti.Slug
	}
	name := requesterGreeting(p)
	msg.TemplateID = ticketReplyNoticeText
	msg.Data = map[string]any{"name": name, "business_name": business, "ticket_id": number}
	// No fallback template: until Meta approves this one the send fails and WhatsApp is skipped for
	// the reply, as for every _btn template (policy section 1); the email copy still goes.
	md["template_name"] = ticketReplyMetaTemplate
	md["template_language"] = "en_US"
	md["template_params"] = []string{name, business, number}
	md["template_button_param"] = suffix
	return msg, ""
}

// ticketTrackSuffix is the template button's path on the ticketing domain: the public ticket page,
// support/track?org=<tenant slug>&ticket=<ticket number>, where the requester confirms their email
// with a code and reads and answers the ticket. ticketing-api's events carry no link or token for
// it, so it is built from the tenant's slug and the ticket number, the two values the page takes
// (ticketing-ui src/app/support/(site)/track). The shared app serves every tenant under its slug, so
// a tenant on its own ticketing domain gets the same page here. "" when either value is missing.
func ticketTrackSuffix(ti *tenantInfo, number string) string {
	if ti == nil || ti.Slug == "" || number == "" {
		return ""
	}
	return "support/track?" + url.Values{"org": {ti.Slug}, "ticket": {number}}.Encode()
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
		zap.String("ticket_number", ticketText(evt.Payload, "ticket_number")), zap.String("template_id", msg.TemplateID))
	return nil
}
