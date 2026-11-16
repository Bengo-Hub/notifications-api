package main

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/preferences"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

func projectTaskEvent(eventType string, extra map[string]any) projectsEvent {
	p := map[string]any{
		"project_id":        "0b6c2b8e",
		"project_name":      "Website relaunch",
		"task_id":           "7d1f3a2c",
		"task_title":        "Update the pricing page",
		"actor_name":        "Brian Otieno",
		"recipient_user_id": "5b0e2d7a-1c3f-4e8a-9b6d-2a7c4f1e0d93",
		"recipient_email":   "amina@example.com",
		"recipient_name":    "Amina",
		"action_path":       "/urban-loft/projects/0b6c2b8e/tasks?task=7d1f3a2c",
	}
	for k, v := range extra {
		p[k] = v
	}
	return projectsEvent{ID: "evt-1", TenantID: "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b", EventType: eventType, Payload: p}
}

var projectTaskTenant = &tenantInfo{Slug: "urban-loft", Country: "KE"}

// manifestParams is the number of body variables of each manifest template.
func manifestParams(t *testing.T) map[string]int {
	t.Helper()
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	placeholder := regexp.MustCompile(`\{\{(\d+)\}\}`)
	out := map[string]int{}
	for _, d := range defs {
		seen := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(d.Body, -1) {
			seen[m[1]] = true
		}
		out[d.Name] = len(seen)
	}
	return out
}

func buildTask(t *testing.T, evt projectsEvent, phone string) partyNotice {
	t.Helper()
	kind := strings.TrimPrefix(evt.EventType, "project.")
	n, ok := buildProjectTaskNotice(evt, kind, projectTaskNotices[kind], projectTaskTenant, "", phone, nil, nil)
	if !ok {
		t.Fatalf("%s: recipient not reachable", evt.EventType)
	}
	return n
}

// A mention reaches the recipient with the excerpt, a link into the projects app and the WhatsApp
// template whose parameters match the manifest.
func TestProjectTaskMentionedNotice(t *testing.T) {
	params := manifestParams(t)
	excerpt := "Can you check\nthe new totals before Friday?"
	n := buildTask(t, projectTaskEvent("task.mentioned", map[string]any{"comment_excerpt": excerpt}), "0711000111")

	if n.TemplateID != "projects/task_mentioned" || n.Subject != "Brian Otieno mentioned you on Update the pricing page" {
		t.Fatalf("template or subject wrong: %s, %q", n.TemplateID, n.Subject)
	}
	if n.IdemBase != "projects-task.mentioned-evt-1" {
		t.Fatalf("idempotency base must be per event: %q", n.IdemBase)
	}
	link := "https://projects.codevertexafrica.com/urban-loft/projects/0b6c2b8e/tasks?task=7d1f3a2c"
	d := n.Data
	if d["action_link"] != link || d["name"] != "Amina" || d["project_name"] != "Website relaunch" ||
		d["comment_excerpt"] != "Can you check the new totals before Friday?" {
		t.Fatalf("data wrong: %v", d)
	}
	if len(n.Parties) != 1 {
		t.Fatalf("one recipient per event, got %d", len(n.Parties))
	}
	wa := n.Parties[0].WhatsApp
	if wa.Name != "projects_task_mentioned_v1_btn" || wa.Button != "urban-loft/projects/0b6c2b8e/tasks?task=7d1f3a2c" {
		t.Fatalf("whatsapp wrong: %+v", wa)
	}
	if want, ok := params[wa.Name]; !ok || want != len(wa.Params) {
		t.Fatalf("%s: manifest has %d params (present %v), code sends %d", wa.Name, want, ok, len(wa.Params))
	}
	for i, v := range wa.Params {
		if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\n\t") {
			t.Fatalf("param %d empty or multi-line: %q", i+1, v)
		}
	}
	out := renderEmail(t, n.TemplateID, d)
	mustContain(t, "mentioned", out, "Hello Amina,", "Update the pricing page", "Website relaunch", "new totals before Friday", link)

	// No excerpt: the short variant, and the email drops the quote.
	short := buildTask(t, projectTaskEvent("task.mentioned", nil), "0711000111")
	if w := short.Parties[0].WhatsApp; w.Name != "projects_task_mentioned_short_v1_btn" || params[w.Name] != len(w.Params) {
		t.Fatalf("short variant wrong: %+v", w)
	}
	if strings.Contains(renderEmail(t, short.TemplateID, short.Data), "blockquote") {
		t.Fatal("email shows an empty quote")
	}
}

func TestProjectTaskAssignedNotice(t *testing.T) {
	params := manifestParams(t)
	n := buildTask(t, projectTaskEvent("task.assigned", map[string]any{"comment_excerpt": "ignored"}), "0711000111")
	if n.TemplateID != "projects/task_assigned" || n.Subject != "Brian Otieno assigned you Update the pricing page" {
		t.Fatalf("template or subject wrong: %s, %q", n.TemplateID, n.Subject)
	}
	if _, ok := n.Data["comment_excerpt"]; ok {
		t.Fatal("an assignment carries no excerpt")
	}
	wa := n.Parties[0].WhatsApp
	if wa.Name != "projects_task_assigned_v1_btn" || params[wa.Name] != len(wa.Params) || wa.Button == "" {
		t.Fatalf("whatsapp wrong: %+v", wa)
	}
	mustContain(t, "assigned", renderEmail(t, n.TemplateID, n.Data), "Brian Otieno assigned you", "Open the task")
}

// With no phone on the user the email still goes and WhatsApp is skipped; with one, both go.
func TestProjectTaskWithoutPhoneSendsEmailOnly(t *testing.T) {
	ctx := context.Background()
	tc := map[string][]string{"projects/task_mentioned": {"email", "whatsapp"}}
	evt := projectTaskEvent("task.mentioned", map[string]any{"comment_excerpt": "Please review"})

	n := buildTask(t, evt, "")
	got := fanOutTargets(ctx, nil, tc, n.TenantID, n.TemplateID, partyContacts(n.Parties[0]))
	if len(got) != 1 || got["email"] != "amina@example.com" {
		t.Fatalf("want email only, got %v", got)
	}
	n = buildTask(t, evt, "0711000111")
	got = fanOutTargets(ctx, nil, tc, n.TenantID, n.TemplateID, partyContacts(n.Parties[0]))
	if got["email"] == "" || got["whatsapp"] != "0711000111" {
		t.Fatalf("want email and whatsapp, got %v", got)
	}

	// No email and no phone: nothing to send.
	noEmail := projectTaskEvent("task.mentioned", map[string]any{"recipient_email": ""})
	if _, ok := buildProjectTaskNotice(noEmail, "task.mentioned", projectTaskNotices["task.mentioned"], projectTaskTenant, "", "", nil, nil); ok {
		t.Fatal("a recipient with no contact must be skipped")
	}
	// The user table's address covers an event without one.
	n, ok := buildProjectTaskNotice(noEmail, "task.mentioned", projectTaskNotices["task.mentioned"], projectTaskTenant, "amina@example.com", "", nil, nil)
	if !ok || n.Parties[0].Email != "amina@example.com" {
		t.Fatalf("user table email not used: %+v", n.Parties)
	}
}

// Both notices are essential: with no tenant or user override they go out on email and WhatsApp.
func TestProjectTaskNoticesOnByDefault(t *testing.T) {
	gate := preferences.NewGate(nil, nil, zap.NewNop())
	tc := map[string][]string{"projects/task_mentioned": {"email", "whatsapp"}, "projects/task_assigned": {"email", "whatsapp"}}
	for _, ev := range []string{"task.mentioned", "task.assigned"} {
		n := buildTask(t, projectTaskEvent(ev, map[string]any{"comment_excerpt": "Please review"}), "0711000111")
		if typ, ok := preferences.Lookup(n.TemplateID); !ok || typ.Class != preferences.ClassEssential || typ.Group != "Support & Projects" {
			t.Fatalf("%s: registry entry wrong: %+v (present %v)", n.TemplateID, typ, ok)
		}
		if got := fanOutTargets(context.Background(), gate, tc, n.TenantID, n.TemplateID, partyContacts(n.Parties[0])); len(got) != 2 {
			t.Fatalf("%s: default delivery = %v, want email and whatsapp", ev, got)
		}
	}
}

// A tenant with its own projects URL keeps the button on the shared app's domain with the same path.
func TestProjectTaskLinkUsesTenantProjectsURL(t *testing.T) {
	ti := &tenantInfo{Slug: "urban-loft", ServiceURLs: map[string]string{"projects": "https://work.urbanloft.example/"}}
	link := projectTaskLink(ti, "/urban-loft/projects/p1/tasks?task=t1")
	if link != "https://work.urbanloft.example/urban-loft/projects/p1/tasks?task=t1" {
		t.Fatalf("link %q", link)
	}
	if s := buttonURLSuffix(link); s != "urban-loft/projects/p1/tasks?task=t1" {
		t.Fatalf("suffix %q", s)
	}
	if projectTaskLink(ti, "") != "" {
		t.Fatal("no path must mean no link")
	}
}
