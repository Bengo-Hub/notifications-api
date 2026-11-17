package main

import (
	"os"
	"strings"
	"testing"
	"text/template"
	"time"
)

var waNow = time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)

// replyPayload is ticket.comment_added for an agent reply on a WhatsApp ticket.
func replyPayload(open bool, expires string) map[string]any {
	return map[string]any{
		"ticket_number":              "TKT-000042",
		"channel":                    "whatsapp",
		"author_type":                "agent",
		"requester_phone":            "+254712345678",
		"requester_email":            "",
		"comment":                    "We have sent it again.",
		"whatsapp_text":              "We have sent it again.\n\n- Check M-Pesa",
		"whatsapp_window_open":       open,
		"whatsapp_window_expires_at": expires,
		"whatsapp_reply_to":          "wamid.7",
		"phone_number_id":            "pn-1",
	}
}

func replyEvent(p map[string]any) ticketingEvent {
	return ticketingEvent{ID: "e1", TenantID: "t1", EventType: "ticket.comment_added", Payload: p}
}

// Inside the window the reply goes as free-form text (no template name) to the requester's phone,
// from the tenant's own number, with its line breaks.
func TestTicketWhatsAppReplyInsideWindow(t *testing.T) {
	msg, skip := ticketWhatsAppReply(replyEvent(replyPayload(true, "2026-11-17T20:00:00Z")), &tenantInfo{Country: "KE"}, waNow)
	if skip != "" {
		t.Fatalf("skipped: %s", skip)
	}
	if msg.Channel != "whatsapp" || msg.TemplateID != ticketWhatsAppTemplate || msg.SenderScope != "tenant" ||
		len(msg.To) != 1 || msg.To[0] != "+254712345678" {
		t.Fatalf("msg = %+v", msg)
	}
	if _, ok := msg.Metadata["template_name"]; ok {
		t.Fatal("a reply inside the window must be free-form text, never a template")
	}
	if msg.Data["comment"] != "We have sent it again.\n\n- Check M-Pesa" || msg.Data["ticket_id"] != "TKT-000042" {
		t.Fatalf("data = %v", msg.Data)
	}
	if _, ok := msg.Data["action_link"]; ok {
		t.Fatal("no link: it would become an interactive button with a 1024 character body")
	}
	if msg.IdempotencyKey != "ticketing-ticket.comment_added-e1" {
		t.Fatalf("idempotency key %q", msg.IdempotencyKey)
	}
}

// Outside the window nothing goes on WhatsApp: no approved template carries a ticket reply.
func TestTicketWhatsAppReplyOutsideWindow(t *testing.T) {
	cases := map[string]map[string]any{
		"closed by ticketing":  replyPayload(false, "2026-11-16T20:00:00Z"),
		"expired while queued": replyPayload(true, "2026-11-17T08:59:00Z"),
		"no window flag":       {"channel": "whatsapp", "author_type": "agent", "requester_phone": "+254712345678", "comment": "Hi"},
	}
	for name, p := range cases {
		if _, skip := ticketWhatsAppReply(replyEvent(p), nil, waNow); skip != ticketWhatsAppWindowClosed {
			t.Errorf("%s: skip = %q", name, skip)
		}
	}
}

func TestTicketWhatsAppReplySkips(t *testing.T) {
	email := replyPayload(true, "")
	email["channel"] = "email"
	if _, skip := ticketWhatsAppReply(replyEvent(email), nil, waNow); skip != ticketWhatsAppNotWhatsApp {
		t.Errorf("email ticket: %q", skip)
	}
	noPhone := replyPayload(true, "")
	noPhone["requester_phone"] = ""
	if _, skip := ticketWhatsAppReply(replyEvent(noPhone), nil, waNow); skip != ticketWhatsAppNoPhone {
		t.Errorf("no phone: %q", skip)
	}
	empty := replyPayload(true, "")
	empty["whatsapp_text"], empty["comment"] = "", ""
	if _, skip := ticketWhatsAppReply(replyEvent(empty), nil, waNow); skip != ticketWhatsAppNoText {
		t.Errorf("empty reply: %q", skip)
	}
}

// The text body renders the reply and the ticket reference, and carries no link.
func TestTicketReplyWhatsAppTemplate(t *testing.T) {
	raw, err := os.ReadFile("../../templates/whatsapp/" + ticketWhatsAppTemplate + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	tpl := template.Must(template.New("msg").Parse(string(raw)))
	var b strings.Builder
	if err := tpl.Execute(&b, map[string]any{"comment": "Sent again.", "ticket_id": "TKT-000042"}); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "Sent again.\n\n_Ticket TKT-000042_" {
		t.Fatalf("rendered %q", got)
	}
}
