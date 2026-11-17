package main

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

var portalTenant = &tenantInfo{Name: "Urban Loft Builders", Slug: "urban-loft", Country: "KE"}

func portalEvent(eventType string, p map[string]any) projectsEvent {
	base := map[string]any{"project_id": "0b6c2b8e", "project_name": "Westlands fit-out", "actor_name": "Brian Otieno"}
	for k, v := range p {
		base[k] = v
	}
	return projectsEvent{ID: "evt-9", TenantID: "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b", EventType: eventType, Payload: base}
}

func buildPortal(t *testing.T, evt projectsEvent, userEmail, userPhone string) partyNotice {
	t.Helper()
	kind := strings.TrimPrefix(evt.EventType, "project.")
	pn, ok := projectPortalNotices[kind]
	if !ok {
		t.Fatalf("%s is not a portal notice", kind)
	}
	n, reachable := buildProjectPortalNotice(evt, kind, pn, portalTenant, userEmail, userPhone, nil, nil)
	if !reachable {
		t.Fatalf("%s: recipient not reachable", kind)
	}
	return n
}

// Every portal notice uses a manifest template with matching parameters, a button into the right
// app, an essential registry entry, and an email that renders its key facts.
func TestProjectPortalNotices(t *testing.T) {
	params := manifestParams(t)
	cases := []struct {
		event, wa, button, target, subject string
		payload                            map[string]any
		emailHas                           []string
	}{
		{
			event: "project.client.invited", wa: "projects_client_invited_v2_btn", target: messaging.TargetCustomer,
			button: "urban-loft/portal/projects/0b6c2b8e", subject: "Urban Loft Builders invited you to follow Westlands fit-out",
			payload: map[string]any{"recipient_name": "Joan", "recipient_email": "joan@example.com", "recipient_phone": "0711000222",
				"action_path": "/urban-loft/portal/projects/0b6c2b8e", "portal_path": "/urban-loft/portal"},
			emailHas: []string{"Hello Joan,", "Urban Loft Builders has invited you", "joan@example.com", "Open the client portal"},
		},
		{
			event: "project.milestone.ready_for_review", wa: "projects_milestone_review_v1_btn", target: messaging.TargetCustomer,
			button: "urban-loft/portal/projects/0b6c2b8e?milestone=m1", subject: "Ceiling is ready for your review",
			payload: map[string]any{"recipient_name": "Joan", "recipient_user_id": "5b0e2d7a-1c3f-4e8a-9b6d-2a7c4f1e0d93",
				"recipient_email": "joan@example.com", "milestone_name": "Ceiling", "target_date": "2026-12-01",
				"action_path": "/urban-loft/portal/projects/0b6c2b8e?milestone=m1"},
			emailHas: []string{"Ceiling", "2026-12-01", "Review the milestone"},
		},
		{
			event: "project.milestone.approved", wa: "projects_milestone_approved_v1_btn", target: messaging.TargetStaff,
			button: "urban-loft/projects/0b6c2b8e/milestones?milestone=m1", subject: "Joan Client approved Ceiling",
			payload: map[string]any{"recipient_name": "Amina", "recipient_email": "amina@example.com", "actor_name": "Joan Client",
				"milestone_name": "Ceiling", "comment_excerpt": "Looks\ngreat", "action_path": "/urban-loft/projects/0b6c2b8e/milestones?milestone=m1"},
			emailHas: []string{"Joan Client approved", "Looks great", "Open the milestone"},
		},
		{
			event: "project.milestone.changes_requested", wa: "projects_milestone_changes_v1_btn", target: messaging.TargetStaff,
			button: "urban-loft/projects/0b6c2b8e/milestones?milestone=m1", subject: "Joan Client asked for changes to Ceiling",
			payload: map[string]any{"recipient_name": "Amina", "recipient_email": "amina@example.com", "actor_name": "Joan Client",
				"milestone_name": "Ceiling", "comment_excerpt": "Move the lights", "action_path": "/urban-loft/projects/0b6c2b8e/milestones?milestone=m1"},
			emailHas: []string{"asked for changes", "Move the lights"},
		},
	}
	gate := preferences.NewGate(nil, nil, zap.NewNop())
	for _, c := range cases {
		n := buildPortal(t, portalEvent(c.event, c.payload), "", "0711000111")
		if n.Subject != c.subject {
			t.Errorf("%s: subject %q", c.event, n.Subject)
		}
		party := n.Parties[0]
		if party.Target != c.target || party.WhatsApp.Name != c.wa || party.WhatsApp.Button != c.button {
			t.Errorf("%s: party %+v whatsapp %+v", c.event, party, party.WhatsApp)
		}
		if want, ok := params[c.wa]; !ok || want != len(party.WhatsApp.Params) {
			t.Errorf("%s: manifest has %d params (present %v), code sends %d", c.wa, want, ok, len(party.WhatsApp.Params))
		}
		for i, v := range party.WhatsApp.Params {
			if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\n\t") {
				t.Errorf("%s param %d empty or multi-line: %q", c.wa, i+1, v)
			}
		}
		if typ, ok := preferences.Lookup(n.TemplateID); !ok || typ.Class != preferences.ClassEssential || typ.Group != "Support & Projects" {
			t.Errorf("%s: registry entry %+v (present %v)", n.TemplateID, typ, ok)
		}
		tc := map[string][]string{n.TemplateID: {"email", "whatsapp"}}
		if got := fanOutTargets(context.Background(), gate, tc, n.TenantID, n.TemplateID, partyContacts(party)); len(got) != 2 {
			t.Errorf("%s: default delivery %v, want email and whatsapp", c.event, got)
		}
		mustContain(t, c.event, renderEmail(t, n.TemplateID, n.Data), c.emailHas...)
	}
}

// The invite reaches the contact by the payload's own phone; an approval without a comment uses
// the short template; a recipient with no contact at all is skipped.
func TestProjectPortalNoticeEdges(t *testing.T) {
	params := manifestParams(t)
	invite := buildPortal(t, portalEvent("project.client.invited", map[string]any{
		"recipient_email": "joan@example.com", "recipient_phone": "0722000333", "action_path": "/urban-loft/portal/projects/p1",
	}), "", "0711000111")
	if invite.Parties[0].Phone != "0722000333" {
		t.Errorf("invite phone %q, want the contact's", invite.Parties[0].Phone)
	}
	short := buildPortal(t, portalEvent("project.milestone.approved", map[string]any{
		"recipient_email": "amina@example.com", "milestone_name": "Ceiling", "action_path": "/urban-loft/projects/p1/milestones?milestone=m1",
	}), "", "")
	if w := short.Parties[0].WhatsApp; w.Name != "projects_milestone_approved_short_v1_btn" || params[w.Name] != len(w.Params) {
		t.Errorf("short approval %+v", w)
	}
	if strings.Contains(renderEmail(t, short.TemplateID, short.Data), "blockquote") {
		t.Error("an approval without a comment shows an empty quote")
	}
	none := portalEvent("project.milestone.ready_for_review", map[string]any{"recipient_email": ""})
	if _, ok := buildProjectPortalNotice(none, "milestone.ready_for_review", projectPortalNotices["milestone.ready_for_review"], portalTenant, "", "", nil, nil); ok {
		t.Error("a recipient with no contact must be skipped")
	}
}
