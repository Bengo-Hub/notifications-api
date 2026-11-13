package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// qaTestEvent decodes an envelope the way projects-api's outbox publishes it.
func qaTestEvent(t *testing.T, raw string) qaEvent {
	t.Helper()
	var e qaEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// qaCommon is projects-api sessionPayload for a session; scope and the event fields are added.
func qaCommon(scope, extra string) string {
	subject := `"subject_kind":"general","subject_ref":""`
	if scope == "platform" {
		subject = `"subject_kind":"service","subject_ref":"hadia"`
	}
	return `{"id":"8d1f0d4e-5a8f-4c1e-9f62-6a3c2b1e0a11","tenant_id":"6f1c0a2e-0000-4000-8000-000000000001","aggregate_type":"project",
		"aggregate_id":"0b7e0c9a-1111-4222-8333-444455556666","event_type":"qa.x","payload":{
		"session_id":"0b7e0c9a-1111-4222-8333-444455556666","tenant_id":"6f1c0a2e-0000-4000-8000-000000000001",
		"tenant_slug":"acme","tenant_name":"Acme Traders","session_title":"Release 1 acceptance","status":"open",
		"scope":"` + scope + `",` + subject + `,"owner_id":"9a9a9a9a-0000-4000-8000-000000000009",
		"link":"https://projects.codevertexafrica.com/qa/sessions/0b7e0c9a-1111-4222-8333-444455556666"` + extra + `}}`
}

func renderQA(t *testing.T, template string, data map[string]any) string {
	t.Helper()
	d := make(map[string]any, len(data)+1)
	for k, v := range data {
		d[k] = v
	}
	d["name"] = "Wanjiru"
	html := renderEmail(t, template, d)
	if strings.Contains(html, "<no value>") {
		t.Errorf("%s renders a missing value:\n%s", template, html)
	}
	noBlankLabels(t, template, html)
	return html
}

// Routing: a platform session reaches ops, a tenant session (or one without a scope) never does.
func TestQARouting(t *testing.T) {
	sev := func(scope string, n int) qaPayload { return qaPayload{Scope: scope, Severity: n} }
	cases := []struct {
		name string
		kind string
		p    qaPayload
		want qaRoute
	}{
		{"invite platform", qaKindInvite, sev("platform", 0), qaRoute{Platform: true, Template: qaInviteTemplate, Invitee: true}},
		{"invite tenant", qaKindInvite, sev("tenant", 0), qaRoute{Template: qaInviteTemplate, Invitee: true}},
		{"failed platform severity 1", qaKindFailed, sev("platform", 1), qaRoute{Platform: true, Ops: true, OpsTemplate: qaOpsResultFailedTemplate, OpsWhatsApp: true}},
		{"failed platform severity 2", qaKindFailed, sev("platform", 2), qaRoute{Platform: true, Ops: true, OpsTemplate: qaOpsResultFailedTemplate}},
		{"failed tenant severity 1", qaKindFailed, sev("tenant", 1), qaRoute{Template: qaResultFailedTemplate, Team: true}},
		{"failed without scope", qaKindFailed, sev("", 1), qaRoute{Template: qaResultFailedTemplate, Team: true}},
		{"closed platform", qaKindClosed, sev("platform", 0), qaRoute{Platform: true, Template: qaSessionClosedTemplate, OwnerSigners: true, Ops: true, OpsTemplate: qaOpsSessionTemplate}},
		{"closed tenant", qaKindClosed, sev("tenant", 0), qaRoute{Template: qaSessionClosedTemplate, OwnerSigners: true}},
		{"accepted platform", qaKindAccepted, sev("Platform", 0), qaRoute{Platform: true, Template: qaSessionDecidedTemplate, OwnerSigners: true, Ops: true, OpsTemplate: qaOpsSessionTemplate}},
		{"accepted tenant", qaKindAccepted, sev("tenant", 0), qaRoute{Template: qaSessionDecidedTemplate, OwnerSigners: true}},
		{"rejected platform", qaKindRejected, sev("platform", 0), qaRoute{Platform: true, Template: qaSessionDecidedTemplate, OwnerSigners: true, Ops: true, OpsTemplate: qaOpsSessionTemplate}},
		{"rejected tenant", qaKindRejected, sev("tenant", 0), qaRoute{Template: qaSessionDecidedTemplate, OwnerSigners: true}},
	}
	for _, c := range cases {
		got, ok := planQA(c.kind, c.p)
		if !ok || got != c.want {
			t.Errorf("%s: got %+v (ok %v), want %+v", c.name, got, ok, c.want)
		}
		if !c.p.platform() && (got.Ops || got.OpsWhatsApp) {
			t.Errorf("%s: a tenant event must never reach ops", c.name)
		}
	}
	if _, ok := planQA("session.opened", qaPayload{Scope: "platform"}); ok {
		t.Error("an event without a notice must not route")
	}
}

func TestQAKindAndKey(t *testing.T) {
	if k := qaKind("project.qa.result.failed", ""); k != qaKindFailed {
		t.Errorf("kind from subject %q", k)
	}
	if k := qaKind("", "qa.invite.created"); k != qaKindInvite {
		t.Errorf("kind from event type %q", k)
	}
	e := qaTestEvent(t, qaCommon("tenant", `,"invite_id":"inv-1"`))
	if qaEventKey(e, qaKindInvite) != "8d1f0d4e-5a8f-4c1e-9f62-6a3c2b1e0a11" {
		t.Errorf("key must be the event id, got %q", qaEventKey(e, qaKindInvite))
	}
	e.ID = ""
	if k := qaEventKey(e, qaKindInvite); k != "invite.created:0b7e0c9a-1111-4222-8333-444455556666:inv-1" {
		t.Errorf("fallback key %q", k)
	}
	s := &Subscriber{}
	ctx := context.Background()
	if !s.claimQAEvent(ctx, "evt-1") || s.claimQAEvent(ctx, "evt-1") || !s.claimQAEvent(ctx, "evt-2") {
		t.Error("an event id must be claimed once")
	}
}

func TestQAMergePeople(t *testing.T) {
	got := qaMergePeople([]qaPerson{{Email: "Owner@Acme.co.ke", Name: "Owner"}, {Email: "owner@acme.co.ke", Name: "Again"},
		{Email: " "}, {Email: "not-an-address"}, {Email: "signer@acme.co.ke"}})
	if len(got) != 2 || got[0].Name != "Owner" || got[1].Email != "signer@acme.co.ke" {
		t.Fatalf("merged %+v", got)
	}
}

// Every event renders for both scopes with every optional line and with none of them.
func TestQATemplatesRender(t *testing.T) {
	invite := `,"invite_id":"inv-1","email":"tester@example.com","name":"Wanjiru","role":"signer",
		"invite_link":"https://projects.codevertexafrica.com/qa/AbC123"`
	failed := `,"criterion_id":"c-1","criterion_title":"Owner registers","area_code":"OWN","requirement_ref":"REQ-4",
		"feature_code":"hadia_owner_registration","severity":1,"result_id":"r-1","defect_id":"d-1","ticket_number":"TKT-100",
		"ticket_status":"ticketed","tester_hint":"w***@example.com","raised_by_hand":false`
	closed := `,"accepted":false,"score":72.5,"document_hash":"ab12cd34"`
	decided := `,"reason":"Two must criteria still fail","document_hash":"ab12cd34"`
	for _, scope := range []string{"platform", "tenant"} {
		org := "Acme Traders"
		if scope == "platform" {
			org = qaPlatformName
		}
		// invite.created
		p := qaTestEvent(t, qaCommon(scope, invite)).Payload
		html := renderQA(t, qaInviteTemplate, qaData(qaKindInvite, p, org))
		for _, want := range []string{"You are invited to Release 1 acceptance", "Run by", org, "invited you to this acceptance session", "the role signer",
			"sign the acceptance", "https://projects.codevertexafrica.com/qa/AbC123"} {
			if !strings.Contains(html, want) {
				t.Errorf("%s invite lacks %q:\n%s", scope, want, html)
			}
		}
		if subj := qaSubject(qaKindInvite, p, org); subj != org+" invited you to Release 1 acceptance" {
			t.Errorf("%s invite subject %q", scope, subj)
		}

		// result.failed: the ops email for a platform session, the team email for a tenant one.
		p = qaTestEvent(t, qaCommon(scope, failed)).Payload
		data := qaData(qaKindFailed, p, org)
		tpl := qaResultFailedTemplate
		if scope == "platform" {
			tpl = qaOpsResultFailedTemplate
		}
		html = renderQA(t, tpl, data)
		for _, want := range []string{"Owner registers", "1 (critical)", "OWN", "REQ-4", "hadia_owner_registration", "Ticket TKT-100 opened", "w***@example.com"} {
			if !strings.Contains(html, want) {
				t.Errorf("%s failure lacks %q:\n%s", scope, want, html)
			}
		}
		text := qaOpsText(qaKindFailed, p, data)
		if strings.Contains(text, "\n") || strings.Contains(text, "example.com") || !strings.Contains(text, "severity 1 (critical)") {
			t.Errorf("%s ops text %q", scope, text)
		}

		// session.closed, accepted, rejected: the owner and signers' email, and ops for a platform session.
		for _, k := range []struct{ kind, extra, want string }{
			{qaKindClosed, closed, "Not met"}, {qaKindAccepted, decided, "Accepted"}, {qaKindRejected, decided, "Two must criteria still fail"},
		} {
			p = qaTestEvent(t, qaCommon(scope, k.extra)).Payload
			data = qaData(k.kind, p, org)
			route, _ := planQA(k.kind, p)
			html = renderQA(t, route.Template, data)
			if !strings.Contains(html, k.want) || !strings.Contains(html, "ab12cd34") {
				t.Errorf("%s %s lacks %q:\n%s", scope, k.kind, k.want, html)
			}
			if route.Ops {
				html = renderQA(t, route.OpsTemplate, data)
				if !strings.Contains(html, "QA session "+strings.TrimPrefix(k.kind, "session.")) {
					t.Errorf("%s %s ops headline:\n%s", scope, k.kind, html)
				}
				if strings.Contains(qaOpsText(k.kind, p, data), "\n") {
					t.Errorf("%s %s ops text has a line break", scope, k.kind)
				}
			}
		}
	}
}

// With only the common fields (no link, no ticket, no outcome, no reason) no line renders empty.
func TestQATemplatesRenderBare(t *testing.T) {
	bare := `{"payload":{"session_id":"s-1","session_title":"","scope":"%s"}}`
	for _, scope := range []string{"platform", "tenant"} {
		p := qaTestEvent(t, strings.Replace(bare, "%s", scope, 1)).Payload
		for _, kind := range []string{qaKindInvite, qaKindFailed, qaKindClosed, qaKindAccepted, qaKindRejected} {
			data := qaData(kind, p, "")
			route, _ := planQA(kind, p)
			if route.Template != "" {
				html := renderQA(t, route.Template, data)
				if strings.Contains(html, "Run by") || strings.Contains(html, "Open the session</a>") || strings.Contains(html, "Reason:") {
					t.Errorf("%s %s shows an optional line:\n%s", scope, kind, html)
				}
			}
			if route.Ops {
				renderQA(t, route.OpsTemplate, data)
				if text := qaOpsText(kind, p, data); strings.Contains(text, "\n") || strings.HasSuffix(text, " ") {
					t.Errorf("%s %s ops text %q", scope, kind, text)
				}
			}
		}
	}
}
