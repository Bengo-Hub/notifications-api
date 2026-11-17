package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/bengobox/notifications-api/internal/config"
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

// Outside the window the reply goes as the ticketing_ticket_reply_v1_btn template: the requester's
// name, the business and the ticket number, never the reply text, and the public ticket page as
// the button suffix.
func TestTicketWhatsAppReplyOutsideWindow(t *testing.T) {
	ti := &tenantInfo{Name: "Urban Loft Cafe", Slug: "urban-loft", Country: "KE"}
	withName := replyPayload(false, "2026-11-16T20:00:00Z")
	withName["requester_name"] = "Amina"
	cases := map[string]struct {
		p    map[string]any
		name string
	}{
		"closed by ticketing":  {withName, "Amina"},
		"expired while queued": {replyPayload(true, "2026-11-17T08:59:00Z"), "there"},
		"no window flag": {map[string]any{"channel": "whatsapp", "author_type": "agent", "requester_phone": "+254712345678",
			"comment": "Hi", "ticket_number": "TKT-000042"}, "there"},
	}
	for label, c := range cases {
		msg, skip := ticketWhatsAppReply(replyEvent(c.p), ti, waNow)
		if skip != "" {
			t.Fatalf("%s: skipped: %s", label, skip)
		}
		if msg.Channel != "whatsapp" || msg.TemplateID != ticketReplyNoticeText || msg.SenderScope != "tenant" ||
			len(msg.To) != 1 || msg.To[0] != "+254712345678" {
			t.Fatalf("%s: msg = %+v", label, msg)
		}
		md := msg.Metadata
		if md["template_name"] != "ticketing_ticket_reply_v1_btn" || md["template_language"] != "en_US" {
			t.Fatalf("%s: metadata = %v", label, md)
		}
		if got, want := md["template_params"], []string{c.name, "Urban Loft Cafe", "TKT-000042"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: params = %v, want %v", label, got, want)
		}
		if md["template_button_param"] != "support/track?org=urban-loft&ticket=TKT-000042" {
			t.Errorf("%s: button suffix = %v", label, md["template_button_param"])
		}
		if _, ok := md["template_fallback_name"]; ok {
			t.Errorf("%s: no fallback: an unapproved template skips WhatsApp, the email still goes", label)
		}
		if md["default_dial_code"] == nil || md["service_id"] != "ticketing" {
			t.Errorf("%s: metadata = %v", label, md)
		}
		for _, v := range msg.Data {
			if s, _ := v.(string); strings.Contains(s, "sent it again") || strings.Contains(s, "://") {
				t.Errorf("%s: the reply text and links stay out of the template: %v", label, msg.Data)
			}
		}
		if msg.IdempotencyKey != "ticketing-ticket.comment_added-e1" {
			t.Errorf("%s: idempotency key %q", label, msg.IdempotencyKey)
		}
	}
}

// Without the tenant's slug or the ticket number there is no button, and a _btn template never
// goes without it: nothing is sent on WhatsApp.
func TestTicketWhatsAppReplyOutsideWindowNeedsLink(t *testing.T) {
	closed := replyPayload(false, "")
	if _, skip := ticketWhatsAppReply(replyEvent(closed), nil, waNow); skip != ticketWhatsAppNoLink {
		t.Errorf("no tenant: %q", skip)
	}
	if _, skip := ticketWhatsAppReply(replyEvent(closed), &tenantInfo{Name: "Urban Loft Cafe"}, waNow); skip != ticketWhatsAppNoLink {
		t.Errorf("no slug: %q", skip)
	}
	closed["ticket_number"] = ""
	if _, skip := ticketWhatsAppReply(replyEvent(closed), &tenantInfo{Slug: "urban-loft"}, waNow); skip != ticketWhatsAppNoLink {
		t.Errorf("no ticket number: %q", skip)
	}
	// A tenant with no name is named by its slug.
	closed["ticket_number"] = "TKT-7"
	msg, _ := ticketWhatsAppReply(replyEvent(closed), &tenantInfo{Slug: "urban-loft"}, waNow)
	if got := msg.Metadata["template_params"]; !reflect.DeepEqual(got, []string{"there", "urban-loft", "TKT-7"}) {
		t.Errorf("params = %v", got)
	}
}

// The switch: only an agent reply with TICKETING_WHATSAPP_REPLIES on goes to WhatsApp at all.
func TestTicketWhatsAppSwitch(t *testing.T) {
	off := &config.Config{}
	on := &config.Config{}
	on.Services.TicketingWhatsAppReplies = true
	reply := replyEvent(replyPayload(false, ""))
	if ticketWhatsAppOn(off, reply) {
		t.Error("switch off: ticket replies go by email only")
	}
	if !ticketWhatsAppOn(on, reply) {
		t.Error("switch on: an agent reply goes on WhatsApp too")
	}
	if ticketWhatsAppOn(on, ticketingEvent{EventType: "ticket.status_changed"}) {
		t.Error("only ticket.comment_added has a WhatsApp copy")
	}
	if ticketWhatsAppOn(nil, reply) {
		t.Error("no config: off")
	}
}

// The template's text body for the delivery log has the same words and no link.
func TestTicketReplyNoticeText(t *testing.T) {
	raw, err := os.ReadFile("../../templates/whatsapp/" + ticketReplyNoticeText + ".txt")
	if err != nil {
		t.Fatal(err)
	}
	tpl := template.Must(template.New("msg").Parse(string(raw)))
	var b strings.Builder
	if err := tpl.Execute(&b, map[string]any{"name": "Amina", "business_name": "Urban Loft Cafe", "ticket_id": "TKT-000042"}); err != nil {
		t.Fatal(err)
	}
	want := "Hi Amina,\n\nThe support team at Urban Loft Cafe has posted a new reply on your support request *TKT-000042*.\n\nOpen the request to read the reply."
	if got := strings.TrimSpace(b.String()); got != want {
		t.Fatalf("rendered %q", got)
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
