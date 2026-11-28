package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

var testTicketTenant = &tenantInfo{Slug: "shop", ContactEmail: "info@shop.example", SupportEmail: "support@shop.example"}

// decodeTicketEvent runs a payload through the same JSON decoding the consumer uses, so numbers
// arrive as float64 and lists as []any exactly as they do from NATS.
func decodeTicketEvent(t *testing.T, eventType string, payload map[string]any) ticketingEvent {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"id": "e-1", "tenant_id": "tn-1", "event_type": eventType, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	var evt ticketingEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatal(err)
	}
	return evt
}

// requesterContactLeaks reports any requester detail found in an agent alert's data or rendered body.
func requesterContactLeaks(data map[string]any, body string) []string {
	var leaks []string
	for _, v := range []string{"jane@example.com", "+254700000001", "Jane Requester"} {
		if strings.Contains(body, v) {
			leaks = append(leaks, v)
		}
		for _, d := range data {
			if s, ok := d.(string); ok && strings.Contains(s, v) {
				leaks = append(leaks, v)
			}
		}
	}
	return leaks
}

// The payload ticketing-api's TicketMentionedPayload builds (tickets/mentions.go).
func mentionPayload() map[string]any {
	return map[string]any{
		"ticket_id": "t-9", "ticket_number": "TKT-000042", "subject": "Printer down",
		"comment_id": "c-1", "comment_type": "internal", "comment": "@Wanjiru can you check the toner?",
		"author_id": "a-1", "agent_id": "a-2", "agent_email": "wanjiru@shop.example",
	}
}

func TestTicketMentionedGoesToTheMentionedAgent(t *testing.T) {
	evt := decodeTicketEvent(t, "ticket.mentioned", mentionPayload())
	m, ok := ticketingMappings[evt.EventType]
	if !ok {
		t.Fatal("ticket.mentioned has no mapping")
	}
	if m.Skip(evt.Payload) {
		t.Fatal("a mention of another agent is sent")
	}
	to, target, replyTo, ok := ticketRecipients(m, evt.Payload, testTicketTenant)
	if !ok || len(to) != 1 || to[0] != "wanjiru@shop.example" || target != messaging.TargetStaff || replyTo != "" {
		t.Errorf("route: %v %q %q %v", to, target, replyTo, ok)
	}
	if got := ticketEmailSubject(m, evt.Payload); got != "You were mentioned on ticket TKT-000042" {
		t.Errorf("subject %q", got)
	}
	d := m.DataBuilder(evt.Payload, testTicketLinks)
	if d["action_link"] != "https://ticketing.example.com/acme/tickets/t-9" || d["internal_note"] != true {
		t.Errorf("data %v", d)
	}
	out := renderEmail(t, m.TemplateID, d)
	mustContain(t, "mention", out, "TKT-000042", "can you check the toner?", "internal note", "https://ticketing.example.com/acme/tickets/t-9")
}

func TestTicketMentionedSkips(t *testing.T) {
	m := ticketingMappings["ticket.mentioned"]
	self := mentionPayload()
	self["agent_id"] = "a-1"
	if !m.Skip(self) {
		t.Error("an agent mentioning themselves is not emailed")
	}
	// No address means nobody: a mention never falls back to the support mailbox.
	noEmail := mentionPayload()
	noEmail["agent_email"] = ""
	if to, _, _, ok := ticketRecipients(m, noEmail, testTicketTenant); ok {
		t.Errorf("a mention without agent_email went to %v", to)
	}
}

// The payload TicketMergedPayload builds: the source ticket's shared payload plus the target.
func mergedPayload() map[string]any {
	return map[string]any{
		"ticket_id": "t-1", "ticket_number": "TKT-000007", "subject": "Card declined", "status": "closed",
		"priority": "medium", "channel": "web", "source": "", "agent_id": "",
		"requester_name": "Jane", "requester_email": "jane@example.com", "requester_phone": "",
		"merged_into_id": "t-2", "merged_into_number": "TKT-000005",
	}
}

func TestTicketMergedTellsTheRequester(t *testing.T) {
	evt := decodeTicketEvent(t, "ticket.merged", mergedPayload())
	m := ticketingMappings[evt.EventType]
	if m.Skip(evt.Payload) {
		t.Fatal("a merge with a target is sent")
	}
	to, target, replyTo, ok := ticketRecipients(m, evt.Payload, testTicketTenant)
	if !ok || len(to) != 1 || to[0] != "jane@example.com" || target != messaging.TargetCustomer || replyTo != "support@shop.example" {
		t.Errorf("route: %v %q %q %v", to, target, replyTo, ok)
	}
	if got := ticketEmailSubject(m, evt.Payload); got != "Your request TKT-000007 is now handled under TKT-000005" {
		t.Errorf("subject %q", got)
	}
	d := m.DataBuilder(evt.Payload, testTicketLinks)
	if d["action_link"] != testTicketLinks.Support || d["name"] != "Jane" {
		t.Errorf("data %v", d)
	}
	mustContain(t, "merged", renderEmail(t, m.TemplateID, d), "#TKT-000007", "#TKT-000005", testTicketLinks.Support)

	noEmail := mergedPayload()
	noEmail["requester_email"] = ""
	if _, _, _, ok := ticketRecipients(m, noEmail, testTicketTenant); ok {
		t.Error("a requester without email must not fall back to the tenant mailbox")
	}
	noTarget := mergedPayload()
	noTarget["merged_into_number"] = ""
	if !m.Skip(noTarget) {
		t.Error("a merge without the target number is not sent")
	}
}

// Sprint 10 payloads are written against the expected shape (sla.breach_warning mirrors
// SLABreachPayload; ticket.escalated is the shared ticket payload plus the escalation), with the
// requester fields present to prove they never reach the alert.
func breachWarningPayload() map[string]any {
	return map[string]any{
		"ticket_id": "t-3", "ticket_number": "TKT-000099", "subject": "Router offline", "priority": "high",
		"status": "open", "agent_id": "a-2", "agent_email": "wanjiru@shop.example", "team_id": "tm-1",
		"target": "resolution", "minutes_left": 30, "due_at": "2026-11-17T12:00:00Z",
		"requester_name": "Jane Requester", "requester_email": "jane@example.com", "requester_phone": "+254700000001",
	}
}

func escalatedPayload() map[string]any {
	return map[string]any{
		"ticket_id": "t-4", "ticket_number": "TKT-000100", "subject": "Payroll stuck", "status": "open",
		"priority": "urgent", "old_priority": "high", "channel": "web", "source": "",
		"agent_id": "a-3", "agent_email": "lead@shop.example", "team_id": "tm-2", "team_name": "Tier 2",
		"escalation_level": 2, "reason": "Resolution SLA breached",
		"requester_name": "Jane Requester", "requester_email": "jane@example.com", "requester_phone": "+254700000001",
	}
}

func TestTicketBreachWarningRecipients(t *testing.T) {
	evt := decodeTicketEvent(t, "sla.breach_warning", breachWarningPayload())
	m, ok := ticketingMappings[evt.EventType]
	if !ok {
		t.Fatal("sla.breach_warning has no mapping")
	}
	to, target, _, ok := ticketRecipients(m, evt.Payload, testTicketTenant)
	if !ok || len(to) != 1 || to[0] != "wanjiru@shop.example" || target != messaging.TargetStaff {
		t.Errorf("assigned: %v %q %v", to, target, ok)
	}

	// Unassigned: the team's supervisors, when the payload carries them.
	unassigned := breachWarningPayload()
	unassigned["agent_email"] = ""
	unassigned["team_emails"] = []any{"sup1@shop.example", " SUP1@shop.example", "sup2@shop.example", "not-an-email"}
	evt = decodeTicketEvent(t, "sla.breach_warning", unassigned)
	if to, _, _, ok := ticketRecipients(m, evt.Payload, testTicketTenant); !ok || strings.Join(to, ",") != "sup1@shop.example,sup2@shop.example" {
		t.Errorf("supervisors: %v %v", to, ok)
	}

	// Nobody named: skipped (the handler logs it), never the support mailbox.
	delete(unassigned, "team_emails")
	if to, _, _, ok := ticketRecipients(m, unassigned, testTicketTenant); ok {
		t.Errorf("an unaddressed warning went to %v", to)
	}
}

func TestTicketBreachWarningContent(t *testing.T) {
	evt := decodeTicketEvent(t, "sla.breach_warning", breachWarningPayload())
	m := ticketingMappings[evt.EventType]
	d := m.DataBuilder(evt.Payload, testTicketLinks)
	if d["breach_label"] != "Resolution" || d["time_left"] != "30 minutes" || d["priority"] != "High" {
		t.Errorf("data %v", d)
	}
	out := renderEmail(t, m.TemplateID, d)
	mustContain(t, "warning", out, "TKT-000099", "in 30 minutes", "17 Nov 2026", "https://ticketing.example.com/acme/tickets/t-3")
	if leaks := requesterContactLeaks(d, out); len(leaks) > 0 {
		t.Errorf("requester contact in an agent alert: %v", leaks)
	}
	if got := ticketEmailSubject(m, evt.Payload); got != "Ticket TKT-000099 is close to its SLA target" {
		t.Errorf("subject %q", got)
	}
	for minutes, want := range map[int]string{0: "", 1: "1 minute", 45: "45 minutes", 60: "1 hour", 120: "2 hours", 90: "1 hour 30 minutes", 121: "2 hours 1 minute"} {
		if got := ticketTimeLeft(minutes); got != want {
			t.Errorf("%d minutes: %q", minutes, got)
		}
	}
	if ticketSLAKindLabel(map[string]any{"target": "first_response"}) != "First response" {
		t.Error("first response label")
	}
}

func TestTicketEscalatedRecipientsAndContent(t *testing.T) {
	evt := decodeTicketEvent(t, "ticket.escalated", escalatedPayload())
	m, ok := ticketingMappings[evt.EventType]
	if !ok {
		t.Fatal("ticket.escalated has no mapping")
	}
	to, target, replyTo, ok := ticketRecipients(m, evt.Payload, testTicketTenant)
	if !ok || len(to) != 1 || to[0] != "lead@shop.example" || target != messaging.TargetStaff || replyTo != "" {
		t.Errorf("assignee: %v %q %q %v", to, target, replyTo, ok)
	}
	d := m.DataBuilder(evt.Payload, testTicketLinks)
	if d["escalation_level"] != "2" || d["priority"] != "Urgent" || d["old_priority"] != "High" || d["team_name"] != "Tier 2" {
		t.Errorf("data %v", d)
	}
	out := renderEmail(t, m.TemplateID, d)
	mustContain(t, "escalated", out, "TKT-000100", "Tier 2", "Urgent (was High)", "Escalation level:</strong> 2")
	if leaks := requesterContactLeaks(d, out); len(leaks) > 0 {
		t.Errorf("requester contact in an agent alert: %v", leaks)
	}

	// Team escalation without an assignee goes to the team address the payload carries.
	team := escalatedPayload()
	team["agent_email"] = ""
	team["team_emails"] = []any{"tier2@shop.example"}
	if to, _, _, ok := ticketRecipients(m, team, testTicketTenant); !ok || len(to) != 1 || to[0] != "tier2@shop.example" {
		t.Errorf("team: %v %v", to, ok)
	}
	delete(team, "team_emails")
	if to, _, _, ok := ticketRecipients(m, team, testTicketTenant); ok {
		t.Errorf("an unaddressed escalation went to %v", to)
	}
}

// The mention alert carries no requester contact either (the payload has none, and nothing is
// added on the way).
func TestTicketMentionedHasNoRequesterContact(t *testing.T) {
	p := mentionPayload()
	p["requester_email"] = "jane@example.com"
	p["requester_phone"] = "+254700000001"
	m := ticketingMappings["ticket.mentioned"]
	d := m.DataBuilder(p, testTicketLinks)
	if leaks := requesterContactLeaks(d, renderEmail(t, m.TemplateID, d)); len(leaks) > 0 {
		t.Errorf("requester contact in a mention alert: %v", leaks)
	}
}

// Every new template is classified for the preference gate: mentions and merges default on,
// the SLA warning and escalation follow the breach and assignment alerts (optional).
func TestTicketNewTemplatesAreRegistered(t *testing.T) {
	for event, class := range map[string]preferences.Class{
		"ticket.mentioned":   preferences.ClassEssential,
		"ticket.merged":      preferences.ClassEssential,
		"sla.breach_warning": preferences.ClassOptional,
		"ticket.escalated":   preferences.ClassOptional,
	} {
		m := ticketingMappings[event]
		ty, ok := preferences.Lookup(m.TemplateID)
		if !ok || ty.Class != class {
			t.Errorf("%s: %s registered %v as %q, want %q", event, m.TemplateID, ok, ty.Class, class)
		}
	}
}

// The existing mappings keep their single recipient and fixed subject.
func TestTicketRecipientsKeepsExistingRoutes(t *testing.T) {
	m := ticketingMappings["sla.resolution_breached"]
	to, _, _, ok := ticketRecipients(m, map[string]any{}, testTicketTenant)
	if !ok || len(to) != 1 || to[0] != "support@shop.example" {
		t.Errorf("unassigned breach: %v %v", to, ok)
	}
	if got := ticketEmailSubject(m, map[string]any{"ticket_number": "TKT-1"}); got != m.EmailSubject {
		t.Errorf("subject %q", got)
	}
}
