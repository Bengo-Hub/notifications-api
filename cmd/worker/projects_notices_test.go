package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

func buildMoreNotice(t *testing.T, evt projectsEvent, userEmail, userPhone string) partyNotice {
	t.Helper()
	kind := strings.TrimPrefix(evt.EventType, "project.")
	en, ok := projectNotices[kind]
	if !ok {
		t.Fatalf("%s is not a client thread or change request notice", kind)
	}
	n, reachable := buildProjectNotice(evt, kind, en, portalTenant, userEmail, userPhone, nil, nil)
	if !reachable {
		t.Fatalf("%s: recipient not reachable", kind)
	}
	return n
}

// checkWhatsAppParams fails when the notice has no WhatsApp text file, its template is not in the
// manifest, the code sends another number of parameters, or a parameter is empty or spans lines.
func checkWhatsAppParams(t *testing.T, params map[string]int, label string, wa *partyWhatsApp) {
	t.Helper()
	if _, err := os.Stat("../../templates/whatsapp/" + label + ".txt"); err != nil {
		t.Errorf("%s: no WhatsApp text file: %v", label, err)
	}
	if wa == nil {
		t.Errorf("%s: no WhatsApp template", label)
		return
	}
	if want, ok := params[wa.Name]; !ok || want != len(wa.Params) {
		t.Errorf("%s: %s has %d params in the manifest (present %v), code sends %d", label, wa.Name, want, ok, len(wa.Params))
	}
	for i, v := range wa.Params {
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\n\t") {
			t.Errorf("%s: %s param %d empty or multi-line: %q", label, wa.Name, i+1, v)
		}
	}
}

// Each notice goes to its one recipient by email and WhatsApp: a manifest template with matching
// parameters and a button into the right page, an essential registry entry, and an email that
// renders its facts. The WhatsApp parameters carry no free text.
func TestProjectMoreNotices(t *testing.T) {
	params := manifestParams(t)
	cases := []struct {
		event, target, subject, link, wa string
		waParams                         []string
		payload                          map[string]any
		emailHas                         []string
	}{
		{
			event: "project.client_thread.posted", target: messaging.TargetStaff,
			subject: "Joan Client wrote on the Westlands fit-out client thread",
			link:    "https://projects.codevertexafrica.com/urban-loft/projects/0b6c2b8e/client-thread",
			wa:      "projects_client_thread_posted_v1_btn", waParams: []string{"Amina", "Joan Client", "Westlands fit-out"},
			payload: map[string]any{"actor_name": "Joan Client", "recipient_name": "Amina", "recipient_email": "amina@example.com",
				"recipient_user_id": "5b0e2d7a-1c3f-4e8a-9b6d-2a7c4f1e0d93", "comment_excerpt": "Can we move\nthe handover?",
				"action_path": "/urban-loft/projects/0b6c2b8e/client-thread"},
			emailHas: []string{"Hello Amina,", "Joan Client wrote on the client thread", "Can we move the handover?", "Open the client thread"},
		},
		{
			event: "project.client_thread.replied", target: messaging.TargetCustomer,
			subject: "Urban Loft Builders replied on Westlands fit-out",
			link:    "https://projects.codevertexafrica.com/urban-loft/portal/projects/0b6c2b8e?tab=messages",
			wa:      "projects_client_thread_replied_v1_btn", waParams: []string{"Joan", "Urban Loft Builders", "Westlands fit-out"},
			payload: map[string]any{"actor_name": "Amina", "recipient_name": "Joan", "recipient_email": "joan@example.com",
				"comment_excerpt": "Yes, Friday works", "action_path": "/urban-loft/portal/projects/0b6c2b8e?tab=messages"},
			emailHas: []string{"Hello Joan,", "Amina from Urban Loft Builders replied", "Yes, Friday works", "Open the messages"},
		},
		{
			event: "project.change_request.review_requested", target: messaging.TargetStaff,
			subject: "Change request to approve: Add a site visit",
			link:    "https://projects.codevertexafrica.com/urban-loft/projects/0b6c2b8e/change-requests?request=cr1",
			wa:      "projects_change_request_review_v1_btn", waParams: []string{"Brian", "Ana", "Add a site visit", "Westlands fit-out"},
			payload: map[string]any{"actor_name": "Ana", "recipient_name": "Brian", "recipient_email": "brian@example.com",
				"title": "Add a site visit", "kind": "schedule", "comment_excerpt": "One more visit before handover",
				"action_path": "/urban-loft/projects/0b6c2b8e/change-requests?request=cr1"},
			emailHas: []string{"Ana raised the change request", "Add a site visit", "schedule", "One more visit before handover", "Review the change request"},
		},
		{
			event: "project.change_request.decided", target: messaging.TargetStaff,
			subject: "Your change request was rejected: Add a site visit",
			link:    "https://projects.codevertexafrica.com/urban-loft/projects/0b6c2b8e/change-requests?request=cr1",
			wa:      "projects_change_request_decided_v1_btn", waParams: []string{"Ana", "Add a site visit", "Westlands fit-out", "rejected"},
			payload: map[string]any{"actor_name": "Brian", "recipient_name": "Ana", "recipient_email": "ana@example.com",
				"title": "Add a site visit", "decision": "rejected", "comment_excerpt": "Not in the budget",
				"action_path": "/urban-loft/projects/0b6c2b8e/change-requests?request=cr1"},
			emailHas: []string{"Your change request was rejected", "Brian rejected your change request", "Not in the budget"},
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
		if party.Target != c.target || party.Phone != "0711000111" || party.Channels != nil {
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
		got := fanOutTargets(context.Background(), gate, tc, n.TenantID, n.TemplateID, partyContacts(party))
		if len(got) != 2 || got["email"] == "" || got["whatsapp"] != "0711000111" {
			t.Errorf("%s: default delivery %v, want email and whatsapp", c.event, got)
		}
		if key := fmt.Sprintf("%s-email%s", n.IdemBase, partyIdemSuffix(n, n.Parties[0])); key != "projects-"+strings.TrimPrefix(c.event, "project.")+"-evt-9-email" {
			t.Errorf("%s: idempotency key %q", c.event, key)
		}
		mustContain(t, c.event, renderEmail(t, n.TemplateID, n.Data), c.emailHas...)
	}
}

// The recipient falls back to the local user's email and phone, and the payload's phone wins; an
// approval shows its own wording; without a phone only the email goes; a recipient with neither
// is skipped, while one with only a phone gets WhatsApp.
func TestProjectMoreNoticeEdges(t *testing.T) {
	ctx := context.Background()
	n := buildMoreNotice(t, portalEvent("project.change_request.decided", map[string]any{
		"title": "Extra week", "decision": "approved", "action_path": "/urban-loft/projects/p1/change-requests?request=r1",
	}), "ana@example.com", "")
	if n.Parties[0].Email != "ana@example.com" || n.Subject != "Your change request was approved: Extra week" {
		t.Errorf("approved notice %+v %q", n.Parties[0], n.Subject)
	}
	if w := n.Parties[0].WhatsApp; w.Params[3] != "approved" || w.Params[0] != "there" {
		t.Errorf("approved whatsapp %+v", w)
	}
	tc := map[string][]string{n.TemplateID: {"email", "whatsapp"}}
	if got := fanOutTargets(ctx, nil, tc, n.TenantID, n.TemplateID, partyContacts(n.Parties[0])); len(got) != 1 || got["email"] == "" {
		t.Errorf("no phone: delivery %v, want email only", got)
	}
	out := renderEmail(t, n.TemplateID, n.Data)
	mustContain(t, "approved", out, "Brian Otieno approved your change request", "date shifts")
	if strings.Contains(out, "blockquote") {
		t.Error("a decision without a comment shows an empty quote")
	}

	own := buildMoreNotice(t, portalEvent("project.client_thread.replied", map[string]any{
		"recipient_email": "joan@example.com", "recipient_phone": "0722000333", "action_path": "/urban-loft/portal/projects/p1?tab=messages",
	}), "", "0711000111")
	if own.Parties[0].Phone != "0722000333" {
		t.Errorf("payload phone %q, want the payload's", own.Parties[0].Phone)
	}

	none := portalEvent("project.client_thread.replied", map[string]any{"recipient_email": "joan@example.invalid",
		"action_path": "/urban-loft/portal/projects/p1?tab=messages"})
	if _, ok := buildProjectNotice(none, "client_thread.replied", projectNotices["client_thread.replied"], portalTenant, "", "", nil, nil); ok {
		t.Error("a recipient with no sendable email and no phone must be skipped")
	}
	phoneOnly := buildMoreNotice(t, none, "", "0711000111")
	if got := fanOutTargets(ctx, nil, map[string][]string{phoneOnly.TemplateID: {"email", "whatsapp"}}, phoneOnly.TenantID,
		phoneOnly.TemplateID, partyContacts(phoneOnly.Parties[0])); len(got) != 1 || got["whatsapp"] != "0711000111" {
		t.Errorf("phone only: delivery %v, want whatsapp only", got)
	}
}

// One automation event fans out to each known recipient, by email and WhatsApp, each with its own
// name and an idempotency key per event, channel and recipient; unknown, duplicate and invalid ids
// are skipped, and a person with only a phone still gets WhatsApp.
func TestProjectAutomationNotice(t *testing.T) {
	params := manifestParams(t)
	tenant := uuid.MustParse("0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b")
	ana, ben, ghost, noMail, phoneOnly := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	evt := portalEvent("project.task.automation_notice", map[string]any{
		"task_id": "t1", "task_title": "Wire the lights", "rule_name": "Tell QA", "to_status": "review", "to_status_label": "In review",
		"message": "Ready for QA", "action_path": "/urban-loft/projects/0b6c2b8e/tasks?task=t1",
		"recipient_ids": []any{ana.String(), "not-an-id", ben.String(), ana.String(), ghost.String(), noMail.String(), phoneOnly.String(), uuid.Nil.String()},
	})
	ids := projectAutomationRecipientIDs(evt.Payload)
	if len(ids) != 5 || ids[0] != ana || ids[1] != ben {
		t.Fatalf("recipient ids %v", ids)
	}
	rows := []*ent.User{
		{AuthServiceUserID: ana, TenantID: uuid.New(), Email: "ana@elsewhere.example", FullName: "Ana Elsewhere"},
		{AuthServiceUserID: ana, TenantID: tenant, Email: "ana@example.com", Phone: "0711000111", FullName: "Ana Wanjiru"},
		{AuthServiceUserID: ben, TenantID: tenant, Email: "ben@example.com"},
		{AuthServiceUserID: noMail, TenantID: tenant, Email: "nomail@tenant.local"},
		{AuthServiceUserID: phoneOnly, TenantID: tenant, Email: "phone@tenant.local", Phone: "0722000333", FullName: "Paul"},
	}
	recipients := pickAutomationRecipients(rows, evt.TenantID, ids)
	if len(recipients) != 3 || recipients[0].Email != "ana@example.com" || recipients[0].Phone != "0711000111" || recipients[0].Name != "Ana Wanjiru" ||
		recipients[1].Email != "ben@example.com" || recipients[2].Email != "" || recipients[2].Phone != "0722000333" {
		t.Fatalf("recipients %+v", recipients)
	}
	n, ok := buildProjectAutomationNotice(evt, portalTenant, recipients, nil, nil)
	if !ok || len(n.Parties) != 3 || n.Subject != "Wire the lights moved to In review" {
		t.Fatalf("notice %+v", n)
	}
	keys := map[string]bool{}
	tc := map[string][]string{n.TemplateID: {"email", "whatsapp"}}
	for _, p := range n.Parties {
		if p.Channels != nil || p.Target != messaging.TargetStaff {
			t.Errorf("party %+v", p)
		}
		checkWhatsAppParams(t, params, n.TemplateID, p.WhatsApp)
		if p.WhatsApp.Name != "projects_task_automation_v1_btn" || p.WhatsApp.Button != "urban-loft/projects/0b6c2b8e/tasks?task=t1" ||
			p.WhatsApp.Params[1] != "Wire the lights" || p.WhatsApp.Params[2] != "Westlands fit-out" || p.WhatsApp.Params[3] != "In review" {
			t.Errorf("whatsapp %+v", p.WhatsApp)
		}
		for ch := range fanOutTargets(context.Background(), nil, tc, n.TenantID, n.TemplateID, partyContacts(p)) {
			keys[fmt.Sprintf("%s-%s%s", n.IdemBase, ch, partyIdemSuffix(n, p))] = true
		}
	}
	for _, want := range []string{
		"projects-task.automation_notice-evt-9-email-" + ana.String(),
		"projects-task.automation_notice-evt-9-whatsapp-" + ana.String(),
		"projects-task.automation_notice-evt-9-email-" + ben.String(),
		"projects-task.automation_notice-evt-9-whatsapp-" + phoneOnly.String(),
	} {
		if !keys[want] {
			t.Errorf("missing send %s in %v", want, keys)
		}
	}
	if len(keys) != 4 {
		t.Errorf("sends %v, want 4 (ben has no phone, paul no email)", keys)
	}
	if n.Parties[0].WhatsApp.Params[0] != "Ana Wanjiru" || n.Parties[1].WhatsApp.Params[0] != "there" {
		t.Errorf("names %q %q", n.Parties[0].WhatsApp.Params[0], n.Parties[1].WhatsApp.Params[0])
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
