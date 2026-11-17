package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

func buildEmailNotice(t *testing.T, evt projectsEvent, userEmail string) partyNotice {
	t.Helper()
	kind := strings.TrimPrefix(evt.EventType, "project.")
	en, ok := projectEmailNotices[kind]
	if !ok {
		t.Fatalf("%s is not an email-only notice", kind)
	}
	n, reachable := buildProjectEmailNotice(evt, kind, en, portalTenant, userEmail, nil, nil)
	if !reachable {
		t.Fatalf("%s: recipient not reachable", kind)
	}
	return n
}

// Each email-only notice goes to its one recipient by email alone (no WhatsApp template, even with
// a WhatsApp file present), with an essential registry entry and an email that renders its facts.
func TestProjectEmailNotices(t *testing.T) {
	cases := []struct {
		event, target, subject, link string
		payload                      map[string]any
		emailHas                     []string
	}{
		{
			event: "project.client_thread.posted", target: messaging.TargetStaff,
			subject: "Joan Client wrote on the Westlands fit-out client thread",
			link:    "https://projects.codevertexafrica.com/urban-loft/projects/0b6c2b8e/client-thread",
			payload: map[string]any{"actor_name": "Joan Client", "recipient_name": "Amina", "recipient_email": "amina@example.com",
				"recipient_user_id": "5b0e2d7a-1c3f-4e8a-9b6d-2a7c4f1e0d93", "comment_excerpt": "Can we move\nthe handover?",
				"action_path": "/urban-loft/projects/0b6c2b8e/client-thread"},
			emailHas: []string{"Hello Amina,", "Joan Client wrote on the client thread", "Can we move the handover?", "Open the client thread"},
		},
		{
			event: "project.client_thread.replied", target: messaging.TargetCustomer,
			subject: "Urban Loft Builders replied on Westlands fit-out",
			link:    "https://projects.codevertexafrica.com/urban-loft/portal/projects/0b6c2b8e?tab=messages",
			payload: map[string]any{"actor_name": "Amina", "recipient_name": "Joan", "recipient_email": "joan@example.com",
				"comment_excerpt": "Yes, Friday works", "action_path": "/urban-loft/portal/projects/0b6c2b8e?tab=messages"},
			emailHas: []string{"Hello Joan,", "Amina from Urban Loft Builders replied", "Yes, Friday works", "Open the messages"},
		},
		{
			event: "project.change_request.review_requested", target: messaging.TargetStaff,
			subject: "Change request to approve: Add a site visit",
			link:    "https://projects.codevertexafrica.com/urban-loft/projects/0b6c2b8e/change-requests?request=cr1",
			payload: map[string]any{"actor_name": "Ana", "recipient_name": "Brian", "recipient_email": "brian@example.com",
				"title": "Add a site visit", "kind": "schedule", "comment_excerpt": "One more visit before handover",
				"action_path": "/urban-loft/projects/0b6c2b8e/change-requests?request=cr1"},
			emailHas: []string{"Ana raised the change request", "Add a site visit", "schedule", "One more visit before handover", "Review the change request"},
		},
		{
			event: "project.change_request.decided", target: messaging.TargetStaff,
			subject: "Your change request was rejected: Add a site visit",
			link:    "https://projects.codevertexafrica.com/urban-loft/projects/0b6c2b8e/change-requests?request=cr1",
			payload: map[string]any{"actor_name": "Brian", "recipient_name": "Ana", "recipient_email": "ana@example.com",
				"title": "Add a site visit", "decision": "rejected", "comment_excerpt": "Not in the budget",
				"action_path": "/urban-loft/projects/0b6c2b8e/change-requests?request=cr1"},
			emailHas: []string{"Your change request was rejected", "Brian rejected your change request", "Not in the budget"},
		},
	}
	gate := preferences.NewGate(nil, nil, zap.NewNop())
	for _, c := range cases {
		n := buildEmailNotice(t, portalEvent(c.event, c.payload), "")
		if n.Subject != c.subject {
			t.Errorf("%s: subject %q", c.event, n.Subject)
		}
		party := n.Parties[0]
		if party.Target != c.target || party.WhatsApp != nil || len(n.Parties) != 1 {
			t.Errorf("%s: parties %+v", c.event, n.Parties)
		}
		if n.Data["action_link"] != c.link {
			t.Errorf("%s: link %v", c.event, n.Data["action_link"])
		}
		if typ, ok := preferences.Lookup(n.TemplateID); !ok || typ.Class != preferences.ClassEssential || typ.Group != "Support & Projects" {
			t.Errorf("%s: registry entry %+v (present %v)", n.TemplateID, typ, ok)
		}
		// Even if a WhatsApp file were drafted, these notices offer email only.
		tc := map[string][]string{n.TemplateID: {"email", "whatsapp"}}
		party.Phone = "0711000111"
		got := fanOutTargets(context.Background(), gate, tc, n.TenantID, n.TemplateID, partyContacts(party))
		if len(got) != 1 || got["email"] == "" {
			t.Errorf("%s: default delivery %v, want email only", c.event, got)
		}
		if key := fmt.Sprintf("%s-email%s", n.IdemBase, partyIdemSuffix(n, n.Parties[0])); key != "projects-"+strings.TrimPrefix(c.event, "project.")+"-evt-9-email" {
			t.Errorf("%s: idempotency key %q", c.event, key)
		}
		mustContain(t, c.event, renderEmail(t, n.TemplateID, n.Data), c.emailHas...)
	}
}

// The recipient falls back to the local user's email; an approval shows its own wording; a
// recipient with no address is skipped.
func TestProjectEmailNoticeEdges(t *testing.T) {
	n := buildEmailNotice(t, portalEvent("project.change_request.decided", map[string]any{
		"title": "Extra week", "decision": "approved", "action_path": "/urban-loft/projects/p1/change-requests?request=r1",
	}), "ana@example.com")
	if n.Parties[0].Email != "ana@example.com" || n.Subject != "Your change request was approved: Extra week" {
		t.Errorf("approved notice %+v %q", n.Parties[0], n.Subject)
	}
	out := renderEmail(t, n.TemplateID, n.Data)
	mustContain(t, "approved", out, "Brian Otieno approved your change request", "date shifts")
	if strings.Contains(out, "blockquote") {
		t.Error("a decision without a comment shows an empty quote")
	}
	none := portalEvent("project.client_thread.replied", map[string]any{"recipient_email": "joan@example.invalid"})
	if _, ok := buildProjectEmailNotice(none, "client_thread.replied", projectEmailNotices["client_thread.replied"], portalTenant, "", nil, nil); ok {
		t.Error("a recipient with no sendable email must be skipped")
	}
}

// One automation event fans out to each known recipient, one email each with its own name and an
// idempotency key per event and recipient; unknown, duplicate and invalid ids are skipped.
func TestProjectAutomationNotice(t *testing.T) {
	tenant := uuid.MustParse("0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b")
	ana, ben, ghost, noMail := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	evt := portalEvent("project.task.automation_notice", map[string]any{
		"task_id": "t1", "task_title": "Wire the lights", "rule_name": "Tell QA", "to_status": "review", "to_status_label": "In review",
		"message": "Ready for QA", "action_path": "/urban-loft/projects/0b6c2b8e/tasks?task=t1",
		"recipient_ids": []any{ana.String(), "not-an-id", ben.String(), ana.String(), ghost.String(), noMail.String(), uuid.Nil.String()},
	})
	ids := projectAutomationRecipientIDs(evt.Payload)
	if len(ids) != 4 || ids[0] != ana || ids[1] != ben {
		t.Fatalf("recipient ids %v", ids)
	}
	rows := []*ent.User{
		{AuthServiceUserID: ana, TenantID: uuid.New(), Email: "ana@elsewhere.example", FullName: "Ana Elsewhere"},
		{AuthServiceUserID: ana, TenantID: tenant, Email: "ana@example.com", FullName: "Ana Wanjiru"},
		{AuthServiceUserID: ben, TenantID: tenant, Email: "ben@example.com"},
		{AuthServiceUserID: noMail, TenantID: tenant, Email: "nomail@tenant.local"},
	}
	recipients := pickAutomationRecipients(rows, evt.TenantID, ids)
	if len(recipients) != 2 || recipients[0].Email != "ana@example.com" || recipients[0].Name != "Ana Wanjiru" || recipients[1].Email != "ben@example.com" {
		t.Fatalf("recipients %+v", recipients)
	}
	n, ok := buildProjectAutomationNotice(evt, portalTenant, recipients, nil, nil)
	if !ok || len(n.Parties) != 2 || n.Subject != "Wire the lights moved to In review" {
		t.Fatalf("notice %+v", n)
	}
	keys := map[string]bool{}
	for _, p := range n.Parties {
		if p.WhatsApp != nil || len(p.Channels) != 1 || p.Channels[0] != "email" || p.Target != messaging.TargetStaff {
			t.Errorf("party %+v", p)
		}
		keys[fmt.Sprintf("%s-email%s", n.IdemBase, partyIdemSuffix(n, p))] = true
	}
	if !keys["projects-task.automation_notice-evt-9-email-"+ana.String()] || !keys["projects-task.automation_notice-evt-9-email-"+ben.String()] {
		t.Errorf("idempotency keys %v", keys)
	}
	if typ, ok := preferences.Lookup(n.TemplateID); !ok || typ.Class != preferences.ClassEssential {
		t.Errorf("registry entry %+v (present %v)", typ, ok)
	}
	data := map[string]any{}
	for k, v := range n.Data {
		data[k] = v
	}
	for k, v := range n.Parties[1].Data {
		data[k] = v
	}
	mustContain(t, "automation", renderEmail(t, n.TemplateID, data),
		"Hello there,", "Wire the lights moved to In review", "Tell QA", "Ready for QA", "Open the task")
	if _, ok := buildProjectAutomationNotice(evt, portalTenant, nil, nil, nil); ok {
		t.Error("an automation with no known recipient must send nothing")
	}
}
