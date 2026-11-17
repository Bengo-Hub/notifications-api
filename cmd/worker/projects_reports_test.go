package main

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// The status report and tender notices go to their one recipient by email and WhatsApp: a manifest
// template with matching parameters and a button into the right page, an essential registry entry,
// and an email that renders their facts.
func TestProjectReportAndTenderNotices(t *testing.T) {
	params := manifestParams(t)
	tender := map[string]any{"tender_id": "5d8e2a1b", "tender_number": "TND-2026-0007", "tender_title": "County roads",
		"client_name": "Kisumu County", "project_id": "", "project_name": ""}
	with := func(base map[string]any, more map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		event, target, subject, link, wa string
		waParams                         []string
		payload                          map[string]any
		emailHas                         []string
	}{
		{
			event: "project.status_report.shared", target: messaging.TargetCustomer,
			subject: "Urban Loft Builders shared a status report on Westlands fit-out",
			link:    "https://projects.codevertexafrica.com/urban-loft/portal/projects/0b6c2b8e?tab=reports&report=r1",
			wa:      "projects_status_report_shared_v1_btn", waParams: []string{"Joan", "Urban Loft Builders", "Status report 9 Nov 2026 to 15 Nov 2026", "Westlands fit-out"},
			payload: map[string]any{"recipient_name": "Joan", "recipient_email": "joan@example.com", "report_id": "r1",
				"report_title": "Status report 9 Nov 2026 to 15 Nov 2026", "rag_overall": "amber", "period_start": "2026-11-09", "period_end": "2026-11-15",
				"action_path": "/urban-loft/portal/projects/0b6c2b8e?tab=reports&report=r1"},
			emailHas: []string{"Hello Joan,", "Status report 9 Nov 2026 to 15 Nov 2026", "09 Nov 2026 to 15 Nov 2026", "Amber", "Read the report"},
		},
		{
			event: "project.tender.notice.go_decision", target: messaging.TargetStaff,
			subject: "Bid decision on TND-2026-0007 County roads: no go",
			link:    "https://projects.codevertexafrica.com/urban-loft/tenders/5d8e2a1b",
			wa:      "projects_tender_decision_v1_btn", waParams: []string{"Ben", "TND-2026-0007 County roads", "no go", "Brian Otieno"},
			payload: with(tender, map[string]any{"recipient_name": "Ben", "recipient_email": "ben@example.com", "decision": "no_go",
				"recommendation": "go", "action_path": "/urban-loft/tenders/5d8e2a1b"}),
			emailHas: []string{"Bid decision on TND-2026-0007 County roads: no go", "Kisumu County", "recommended go", "Open the tender"},
		},
		{
			event: "project.tender.notice.section_review", target: messaging.TargetStaff,
			subject: "Section to review: Technical proposal (TND-2026-0007 County roads)",
			link:    "https://projects.codevertexafrica.com/urban-loft/tenders/5d8e2a1b/sections?section=s1",
			wa:      "projects_tender_section_review_v1_btn", waParams: []string{"Ben", "Brian Otieno", "Technical proposal", "TND-2026-0007 County roads"},
			payload: with(tender, map[string]any{"recipient_name": "Ben", "recipient_email": "ben@example.com", "section": "Technical proposal",
				"action_path": "/urban-loft/tenders/5d8e2a1b/sections?section=s1"}),
			emailHas: []string{"waits for your review", "Technical proposal", "Review the section"},
		},
		{
			event: "project.tender.notice.section_reviewed", target: messaging.TargetStaff,
			subject: "Your section Technical proposal was sent back for changes",
			link:    "https://projects.codevertexafrica.com/urban-loft/tenders/5d8e2a1b/sections?section=s1",
			wa:      "projects_tender_section_reviewed_v1_btn", waParams: []string{"Ana", "Technical proposal", "TND-2026-0007 County roads", "sent back for changes"},
			payload: with(tender, map[string]any{"recipient_name": "Ana", "recipient_email": "ana@example.com", "section": "Technical proposal",
				"decision": "changes_requested", "comment_excerpt": "Add the method statement", "action_path": "/urban-loft/tenders/5d8e2a1b/sections?section=s1"}),
			emailHas: []string{"Your section was sent back for changes", "Add the method statement", "send it for review again"},
		},
		{
			event: "project.tender.notice.won", target: messaging.TargetStaff,
			subject: "Tender won: TND-2026-0007 County roads",
			link:    "https://projects.codevertexafrica.com/urban-loft/tenders/5d8e2a1b",
			wa:      "projects_tender_won_v1_btn", waParams: []string{"Ben", "TND-2026-0007 County roads", "Kisumu County"},
			payload: with(tender, map[string]any{"recipient_name": "Ben", "recipient_email": "ben@example.com", "outcome": "won",
				"action_path": "/urban-loft/tenders/5d8e2a1b"}),
			emailHas: []string{"Tender won", "as won", "Open the tender"},
		},
		{
			event: "project.tender.notice.lost", target: messaging.TargetStaff,
			subject: "Tender lost: TND-2026-0007 County roads",
			link:    "https://projects.codevertexafrica.com/urban-loft/tenders/5d8e2a1b",
			wa:      "projects_tender_lost_v1_btn", waParams: []string{"Ben", "TND-2026-0007 County roads", "Kisumu County"},
			payload: with(tender, map[string]any{"recipient_name": "Ben", "recipient_email": "ben@example.com", "outcome": "lost",
				"comment_excerpt": "Price too high", "action_path": "/urban-loft/tenders/5d8e2a1b"}),
			emailHas: []string{"Tender lost", "Price too high"},
		},
		{
			event: "project.tender.notice.converted", target: messaging.TargetStaff,
			subject: "TND-2026-0007 County roads is now the project County roads works",
			link:    "https://projects.codevertexafrica.com/urban-loft/projects/p9",
			wa:      "projects_tender_converted_v1_btn", waParams: []string{"Ben", "TND-2026-0007 County roads", "County roads works"},
			payload: with(tender, map[string]any{"recipient_name": "Ben", "recipient_email": "ben@example.com", "project_id": "p9",
				"project_name": "County roads works", "action_path": "/urban-loft/projects/p9"}),
			emailHas: []string{"is now a project", "County roads works", "Open the project"},
		},
	}
	gate := preferences.NewGate(nil, nil, zap.NewNop())
	for _, c := range cases {
		n := buildMoreNotice(t, portalEvent(c.event, c.payload), "", "0711000111")
		if n.Subject != c.subject {
			t.Errorf("%s: subject %q", c.event, n.Subject)
		}
		if len(n.Parties) != 1 {
			t.Fatalf("%s: parties %+v", c.event, n.Parties)
		}
		party := n.Parties[0]
		if party.Target != c.target || party.Phone != "0711000111" {
			t.Errorf("%s: party %+v", c.event, party)
		}
		wa := party.WhatsApp
		checkWhatsAppParams(t, params, n.TemplateID, wa)
		if wa.Name != c.wa || strings.Join(wa.Params, "|") != strings.Join(c.waParams, "|") ||
			wa.Button != strings.TrimPrefix(c.link, "https://projects.codevertexafrica.com/") {
			t.Errorf("%s: whatsapp %+v", c.event, wa)
		}
		if n.Data["action_link"] != c.link {
			t.Errorf("%s: link %v", c.event, n.Data["action_link"])
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

// An invited client's share notice reaches the phone and address projects-api read from the CRM.
func TestStatusReportShareToInvitedClient(t *testing.T) {
	n := buildMoreNotice(t, portalEvent("project.status_report.shared", map[string]any{
		"recipient_user_id": "", "recipient_email": "joan@example.com", "recipient_phone": "+254700000001",
		"recipient_name": "Joan", "report_title": "Week 46", "action_path": "/urban-loft/portal/projects/0b6c2b8e?tab=reports&report=r1",
	}), "", "")
	if n.Parties[0].Phone != "+254700000001" || n.Parties[0].Email != "joan@example.com" {
		t.Errorf("invited client party %+v", n.Parties[0])
	}
}

// The weekly digest: counts as WhatsApp parameters, lists in the email, My Work as the link, and
// nothing for an empty digest.
func TestProjectDigestNotice(t *testing.T) {
	params := manifestParams(t)
	evt := projectsEvent{ID: "evt-7", TenantID: "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b", EventType: "project.digest.weekly", Payload: map[string]any{
		"recipient_user_id": "u1", "recipient_email": "ana@example.com", "recipient_name": "Ana", "week": "2026-W47",
		"week_start": "2026-11-16", "overdue_count": float64(1), "due_count": float64(2), "milestone_count": float64(0),
		"risk_count": float64(1), "approval_count": float64(1), "action_path": "",
		"overdue":           []any{map[string]any{"title": "Late drawing", "project": "Bridge", "due": "2026-11-10"}},
		"due_this_week":     []any{map[string]any{"title": "Pour deck", "project": "Bridge", "due": "2026-11-18"}},
		"rising_risks":      []any{map[string]any{"title": "Supplier late (score 20)", "project": "Bridge"}},
		"pending_approvals": []any{map[string]any{"title": "Decision 1: Use steel", "project": "Bridge"}},
	}}
	n, ok := buildProjectDigestNotice(evt, portalTenant, "", "0711000111", nil, nil)
	if !ok {
		t.Fatal("digest not reachable")
	}
	wa := n.Parties[0].WhatsApp
	checkWhatsAppParams(t, params, n.TemplateID, wa)
	if wa.Name != "projects_digest_weekly_v1_btn" || strings.Join(wa.Params, "|") != "Ana|Urban Loft Builders|1|2|0|1|1" ||
		wa.Button != "urban-loft/my-work" || n.Data["action_link"] != "https://projects.codevertexafrica.com/urban-loft/my-work" {
		t.Errorf("digest whatsapp %+v link %v", wa, n.Data["action_link"])
	}
	if n.Subject != "Your projects this week (2026-W47)" || n.IdemBase != "projects-digest.weekly-evt-7" {
		t.Errorf("subject %q idem %q", n.Subject, n.IdemBase)
	}
	if typ, ok := preferences.Lookup(n.TemplateID); !ok || typ.Class != preferences.ClassEssential {
		t.Errorf("registry entry %+v (present %v)", typ, ok)
	}
	mustContain(t, "digest", renderEmail(t, n.TemplateID, n.Data),
		"Hello Ana,", "from 16 Nov 2026", "Late drawing (Bridge, due 10 Nov 2026)", "Pour deck", "Supplier late (score 20)", "Decision 1: Use steel", "Open My Work")

	empty := evt
	empty.Payload = map[string]any{"recipient_email": "ana@example.com", "overdue_count": float64(0)}
	if _, ok := buildProjectDigestNotice(empty, portalTenant, "", "", nil, nil); ok {
		t.Error("empty digest built")
	}
}
