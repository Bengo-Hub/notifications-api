package main

import (
	"os"
	"path/filepath"
	"testing"
)

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
		d := m.DataBuilder(payload, "https://support.example.com")
		if d["breach_label"] != label || d["name"] != "Wanjiru" || d["ticket_id"] != "1042" || d["action_link"] != "https://support.example.com/tickets/t-1" {
			t.Errorf("%s data %v", event, d)
		}
	}
	if d := ticketingMappings["sla.resolution_breached"].DataBuilder(map[string]any{"ticket_id": "t-2"}, "https://x"); d["name"] != "Agent" {
		t.Errorf("an unnamed agent is greeted generically: %v", d["name"])
	}
	if _, err := os.Stat(filepath.Join("..", "..", "templates", "email", "ticketing", "ticket_sla_breached.html")); err != nil {
		t.Errorf("email template missing: %v", err)
	}
}
