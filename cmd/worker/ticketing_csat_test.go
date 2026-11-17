package main

import (
	"reflect"
	"strconv"
	"testing"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

const testCSATURL = "https://ticketing.example.com/support/csat?tenant=tn-1&token=tok123"

// The payload TicketCSATRequestedPayload builds (docs/integrations.md in ticketing-api): the
// common ticket keys with the email thread fields, plus the survey link.
func csatPayload() map[string]any {
	return map[string]any{
		"ticket_id": "0b5f8f9e-1111-4c1a-8d52-0a1b2c3d4e5f", "ticket_number": "TKT-000042", "subject": "Printer jam",
		"status": "resolved", "priority": "normal", "channel": "email", "source": "", "agent_id": "",
		"requester_name": "Jane", "requester_email": "jane@example.com", "requester_phone": "",
		"resolved_at": "2026-11-17T09:00:00Z", "survey_channel": "email",
		"csat_url": testCSATURL, "csat_token": "tok123", "csat_expires_at": "2026-11-24T09:00:00Z",
		"email_message_token": "", "email_thread_token": "0b5f8f9e-1111-4c1a-8d52-0a1b2c3d4e5f",
		"email_in_reply_to": "<cust-1@mail.example.com>", "email_references": []any{"<cust-1@mail.example.com>"},
		"email_subject_tag": "[TKT-000042]", "email_reply_subject": "Re: Printer jam [TKT-000042]",
	}
}

func TestTicketCSATGoesToTheRequester(t *testing.T) {
	evt := decodeTicketEvent(t, "ticket.csat_requested", csatPayload())
	m, ok := ticketingMappings[evt.EventType]
	if !ok {
		t.Fatal("ticket.csat_requested has no mapping")
	}
	if m.TemplateID != "ticketing/ticket_csat_survey" {
		t.Errorf("template %q", m.TemplateID)
	}
	if m.Skip(evt.Payload) {
		t.Fatal("a survey with a link is sent")
	}
	to, target, replyTo, ok := ticketRecipients(m, evt.Payload, testTicketTenant)
	if !ok || len(to) != 1 || to[0] != "jane@example.com" || target != messaging.TargetCustomer || replyTo != "support@shop.example" {
		t.Errorf("route: %v %q %q %v", to, target, replyTo, ok)
	}

	msg := ticketMessage(m, evt, to, target, replyTo, testTicketLinks)
	if msg.Channel != "email" || msg.TemplateID != "ticketing/ticket_csat_survey" || msg.ReplyTo != "support@shop.example" {
		t.Errorf("message %+v", msg)
	}
	if msg.IdempotencyKey != "ticketing-ticket.csat_requested-e-1" {
		t.Errorf("idempotency key %q", msg.IdempotencyKey)
	}
	// Threaded onto the ticket's email thread, with the ticket tag on its own subject.
	md := msg.Metadata
	if md["subject"] != "How did we do with your support request? [TKT-000042]" {
		t.Errorf("subject %q", md["subject"])
	}
	if md[metaEmailThreadToken] != "0b5f8f9e-1111-4c1a-8d52-0a1b2c3d4e5f" || md[metaEmailInReplyTo] != "<cust-1@mail.example.com>" {
		t.Errorf("thread metadata %v", md)
	}
	if refs := metadataStrings(md[metaEmailReferences]); !reflect.DeepEqual(refs, []string{"<cust-1@mail.example.com>"}) {
		t.Errorf("references %v", refs)
	}
}

func TestTicketCSATScoreLinks(t *testing.T) {
	m := ticketingMappings["ticket.csat_requested"]
	d := m.DataBuilder(csatPayload(), testTicketLinks)
	if d["csat_url"] != testCSATURL || d["ticket_id"] != "TKT-000042" || d["subject"] != "Printer jam" || d["name"] != "Jane" {
		t.Errorf("data %v", d)
	}
	links, _ := d["score_links"].([]csatScoreLink)
	if len(links) != 5 {
		t.Fatalf("score links %v", d["score_links"])
	}
	for i, l := range links {
		if want := testCSATURL + "&score=" + strconv.Itoa(i+1); l.Score != i+1 || l.URL != want {
			t.Errorf("score %d: %+v, want %s", i+1, l, want)
		}
	}
	if d["expires_at"] != "24 Nov 2026, 12:00 PM" {
		t.Errorf("expires_at %q", d["expires_at"])
	}

	// html/template writes "&" in an attribute as "&amp;".
	out := renderEmail(t, m.TemplateID, d)
	mustContain(t, "csat", out,
		"#TKT-000042", "Printer jam", "Rate your experience", "24 Nov 2026",
		`href="https://ticketing.example.com/support/csat?tenant=tn-1&amp;token=tok123"`,
		`href="https://ticketing.example.com/support/csat?tenant=tn-1&amp;token=tok123&amp;score=1"`,
		`href="https://ticketing.example.com/support/csat?tenant=tn-1&amp;token=tok123&amp;score=5"`,
	)

	// A link without a query string still gets a well formed score parameter.
	if got := csatScoreURL("https://x.example/csat", 3); got != "https://x.example/csat?score=3" {
		t.Errorf("no query: %q", got)
	}
}

func TestTicketCSATSkips(t *testing.T) {
	m := ticketingMappings["ticket.csat_requested"]
	// No requester email: nothing is sent, never the tenant's mailbox.
	noEmail := csatPayload()
	noEmail["requester_email"] = ""
	if to, _, _, ok := ticketRecipients(m, noEmail, testTicketTenant); ok {
		t.Errorf("a survey without a requester email went to %v", to)
	}
	noLink := csatPayload()
	noLink["csat_url"] = ""
	if !m.Skip(noLink) {
		t.Error("a survey without a link is not sent")
	}
	whatsapp := csatPayload()
	whatsapp["survey_channel"] = "whatsapp"
	if !m.Skip(whatsapp) {
		t.Error("only email surveys are sent")
	}
	// The survey never goes on WhatsApp, even with ticket WhatsApp replies on.
	on := &config.Config{}
	on.Services.TicketingWhatsAppReplies = true
	if ticketWhatsAppOn(on, ticketingEvent{EventType: "ticket.csat_requested", Payload: csatPayload()}) {
		t.Error("the survey must not go on WhatsApp")
	}
}

// The survey is classified for the preference gate: on by default, and a tenant can turn it off.
func TestTicketCSATRegistered(t *testing.T) {
	ty, ok := preferences.Lookup("ticketing/ticket_csat_survey")
	if !ok || ty.Class != preferences.ClassEssential || ty.Group != "Support & Projects" {
		t.Errorf("registered %v as %+v", ok, ty)
	}
}
