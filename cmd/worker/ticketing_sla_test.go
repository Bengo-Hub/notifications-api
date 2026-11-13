package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bengobox/notifications-api/internal/messaging"
)

var testTicketLinks = ticketLinks{Console: "https://support.example.com", Support: "https://ticketing.example.com/support"}

// ticketing-api's SLA worker publishes ticketing.sla.first_response_breached and
// ticketing.sla.resolution_breached; both reach the assigned agent with the ticket template.
func TestTicketingSLABreachMappings(t *testing.T) {
	payload := map[string]any{"ticket_id": "t-1", "ticket_number": "1042", "subject": "Printer down", "priority": "high", "agent_name": "Wanjiru"}
	for event, label := range map[string]string{"sla.first_response_breached": "First response", "sla.resolution_breached": "Resolution"} {
		m, ok := ticketingMappings[event]
		if !ok {
			t.Fatalf("%s has no mapping", event)
		}
		if m.TemplateID != "ticketing/ticket_sla_breached" {
			t.Errorf("%s template %q", event, m.TemplateID)
		}
		d := m.DataBuilder(payload, testTicketLinks)
		if d["breach_label"] != label || d["name"] != "Wanjiru" || d["ticket_id"] != "1042" || d["action_link"] != "https://support.example.com/tickets/t-1" {
			t.Errorf("%s data %v", event, d)
		}
	}
	if d := ticketingMappings["sla.resolution_breached"].DataBuilder(map[string]any{"ticket_id": "t-2"}, testTicketLinks); d["name"] != "Agent" {
		t.Errorf("an unnamed agent is greeted generically: %v", d["name"])
	}
	if _, err := os.Stat(filepath.Join("..", "..", "templates", "email", "ticketing", "ticket_sla_breached.html")); err != nil {
		t.Errorf("email template missing: %v", err)
	}
}

// Every ticket event ticketing-api publishes (internal/modules/tickets/events.go) has a mapping
// whose template exists.
func TestTicketingMappingsHaveTemplates(t *testing.T) {
	for _, event := range []string{"ticket.created", "ticket.comment_added", "ticket.status_changed", "ticket.resolved", "ticket.assigned", "sla.first_response_breached", "sla.resolution_breached"} {
		m, ok := ticketingMappings[event]
		if !ok {
			t.Errorf("%s has no mapping", event)
			continue
		}
		if _, err := os.Stat(filepath.Join("..", "..", "templates", "email", m.TemplateID+".html")); err != nil {
			t.Errorf("%s: template %s missing", event, m.TemplateID)
		}
	}
}

// Requester mail goes to the requester with the tenant's support address as Reply-To, never to the
// tenant's mailbox; agent mail goes to the agent, else the support address.
func TestTicketRoute(t *testing.T) {
	kura := &tenantInfo{Slug: "kura", ContactEmail: "info@kura.example", SupportEmail: "support@kura.example"}

	to, target, replyTo, ok := ticketRoute(ticketingMappings["ticket.created"], map[string]any{"requester_email": "jane@example.com"}, kura)
	if !ok || to != "jane@example.com" || target != messaging.TargetCustomer || replyTo != "support@kura.example" {
		t.Errorf("created: %q %q %q %v", to, target, replyTo, ok)
	}
	// Public tickets from before 13 November carry customer_email.
	if to, _, _, _ := ticketRoute(ticketingMappings["ticket.resolved"], map[string]any{"customer_email": "old@example.com"}, kura); to != "old@example.com" {
		t.Errorf("legacy customer_email: %q", to)
	}
	if _, _, _, ok := ticketRoute(ticketingMappings["ticket.resolved"], map[string]any{"requester_phone": "+254700000000"}, kura); ok {
		t.Error("a requester without email must not fall back to the tenant mailbox")
	}

	to, target, replyTo, ok = ticketRoute(ticketingMappings["ticket.assigned"], map[string]any{"agent_email": "agent@kura.example"}, kura)
	if !ok || to != "agent@kura.example" || target != messaging.TargetStaff || replyTo != "" {
		t.Errorf("assigned: %q %q %q %v", to, target, replyTo, ok)
	}
	if to, _, _, _ := ticketRoute(ticketingMappings["sla.resolution_breached"], map[string]any{}, kura); to != "support@kura.example" {
		t.Errorf("unassigned SLA breach goes to support: %q", to)
	}
	// The platform's own tickets answer from support@codevertexafrica.com when nothing is set.
	if _, _, replyTo, _ := ticketRoute(ticketingMappings["ticket.created"], map[string]any{"requester_email": "a@b.c"}, &tenantInfo{Slug: "codevertex"}); replyTo != platformSupportEmail {
		t.Errorf("platform Reply-To: %q", replyTo)
	}
}

// Only agent replies and status changes the requester needs produce mail.
func TestTicketRequesterFilters(t *testing.T) {
	reply := ticketingMappings["ticket.comment_added"]
	if reply.Skip(map[string]any{"author_type": "agent", "comment": "Try restarting it"}) {
		t.Error("an agent reply is sent")
	}
	if !reply.Skip(map[string]any{"author_type": "customer", "comment": "hi"}) {
		t.Error("a customer's own comment is not echoed back")
	}
	status := ticketingMappings["ticket.status_changed"]
	for _, c := range []struct {
		old, new string
		send     bool
	}{
		{"open", "pending_customer", true},
		{"in_progress", "closed", true},
		{"open", "cancelled", true},
		{"resolved", "open", true},
		{"new", "open", false},
		{"open", "in_progress", false},
		{"in_progress", "pending_internal", false},
	} {
		if skip := status.Skip(map[string]any{"old_status": c.old, "new_status": c.new}); skip == c.send {
			t.Errorf("%s to %s: send %v", c.old, c.new, !skip)
		}
	}
	d := status.DataBuilder(map[string]any{"ticket_number": "77", "old_status": "open", "new_status": "pending_customer", "requester_name": "Jane"}, testTicketLinks)
	if d["status"] != "Waiting for your reply" || d["name"] != "Jane" || d["action_link"] != testTicketLinks.Support || d["status_notice"] == "" {
		t.Errorf("status data %v", d)
	}
	if d := ticketingMappings["ticket.created"].DataBuilder(map[string]any{}, testTicketLinks); d["name"] != "there" {
		t.Errorf("unnamed requester: %v", d["name"])
	}
}

// Each comment and status change on a ticket is its own message.
func TestTicketIdempotencyKeyPerEvent(t *testing.T) {
	a := ticketIdempotencyKey(ticketingEvent{ID: "e1", EventType: "ticket.comment_added", AggregateID: "t"})
	b := ticketIdempotencyKey(ticketingEvent{ID: "e2", EventType: "ticket.comment_added", AggregateID: "t"})
	if a == b {
		t.Error("two comments on one ticket share a key")
	}
}
