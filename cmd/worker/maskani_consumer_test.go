package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// TestMaskaniMappingsMatchManifest keeps every maskani mapping sendable: the WhatsApp template
// exists in templates.json with exactly as many body parameters as the mapping sends, a button
// template always has a link path, no parameter is empty (Meta rejects empty parameters), and the
// email and WhatsApp template files exist.
func TestMaskaniMappingsMatchManifest(t *testing.T) {
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	placeholder := regexp.MustCompile(`\{\{(\d+)\}\}`)
	params := map[string]int{}
	for _, d := range defs {
		seen := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(d.Body, -1) {
			seen[m[1]] = true
		}
		params[d.Name] = len(seen)
	}
	sample := map[string]any{
		"name": "Jane", "period": "2026-11", "account_ref": "B07", "unit_code": "B07", "amount": "6450.00",
		"due_date": "10 Nov 2026", "paybill": "4012345", "invoice_number": "INV-1", "receipt": "SJK4H7Q2LM",
		"balance": "0.00", "visitor_name": "Peter", "code": "482915", "valid_from": "2026-10-11T06:00:00Z",
		"valid_to": "2026-10-11T18:00:00Z", "host_name": "Jane", "occurred_at": "2026-10-11T11:32:00Z",
		"event_id": "e1", "buyer": "Jane", "contract_number": "SC-1", "net_price": "7500000.00",
		"severity": "critical", "title": "Fence breached", "number": "INC-1", "incident_id": "i1", "urgent": true,
		"work_order_id": "w1", "priority": "high", "vendor": "Guardforce", "doc_type": "PSRA licence",
		"days_left": 14, "vendor_id": "v1",
	}
	ti := &tenantInfo{Name: "Shaba Village", Slug: "shaba-village"}
	root := filepath.Join("..", "..", "templates")
	for event, m := range maskaniMappings {
		if _, err := os.Stat(filepath.Join(root, "email", m.TemplateID+".html")); err != nil {
			t.Errorf("%s: missing email template %s.html", event, m.TemplateID)
		}
		if m.Subject == nil || m.Subject(sample) == "" {
			t.Errorf("%s: email subject is empty", event)
		}
		if m.WATemplate == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "whatsapp", m.TemplateID+".txt")); err != nil {
			t.Errorf("%s: missing whatsapp body %s.txt", event, m.TemplateID)
		}
		want, ok := params[m.WATemplate]
		if !ok {
			t.Errorf("%s: %q is not in templates.json", event, m.WATemplate)
			continue
		}
		got := m.WAParams(m.Data(sample, ti))
		if len(got) != want {
			t.Errorf("%s: %q expects %d parameters, mapping sends %d", event, m.WATemplate, want, len(got))
		}
		for i, v := range got {
			if strings.TrimSpace(v) == "" {
				t.Errorf("%s: parameter %d is empty", event, i+1)
			}
		}
		if strings.HasSuffix(m.WATemplate, "_btn") && (m.Path == nil || m.Path(sample) == "") {
			t.Errorf("%s: button template %q needs a link path", event, m.WATemplate)
		}
	}
}

func TestMaskaniFormatting(t *testing.T) {
	p := map[string]any{"amount": "6450.00", "at": "2026-10-11T11:32:00Z"}
	if got := mMoney(p, "amount"); !strings.Contains(got, "6,450") {
		t.Errorf("mMoney = %q, want thousands separated", got)
	}
	if got := mTime(p, "at"); got != "11 Oct 14:32" {
		t.Errorf("mTime = %q, want East Africa Time", got)
	}
	if got := withID("works", "id")(map[string]any{}); got != "" {
		t.Errorf("withID without an id = %q, want no link", got)
	}
}
