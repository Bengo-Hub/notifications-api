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
		"days_left": 14, "vendor_id": "v1", "account_id": "a1", "contract_id": "c1", "days_overdue": 9, "document_number": "DOC-1",
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
		if m.WATemplate == "" && m.WAVariant == nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "whatsapp", m.TemplateID+".txt")); err != nil {
			t.Errorf("%s: missing whatsapp body %s.txt", event, m.TemplateID)
		}
		// Every payload shape a mapping can meet: variants pick a template per shape.
		shapes := []map[string]any{sample}
		if m.WAVariant != nil {
			shapes = maskaniBillShapes(sample)
		}
		for _, shape := range shapes {
			name, got := m.WATemplate, []string(nil)
			if m.WAVariant != nil {
				name, got = m.WAVariant(m.Data(shape, ti))
			} else {
				got = m.WAParams(m.Data(shape, ti))
			}
			want, ok := params[name]
			if !ok {
				t.Errorf("%s: %q is not in templates.json", event, name)
				continue
			}
			if len(got) != want {
				t.Errorf("%s: %q expects %d parameters, mapping sends %d", event, name, want, len(got))
			}
			for i, v := range got {
				if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\n\t") {
					t.Errorf("%s: %q parameter %d is empty or multi-line (%q)", event, name, i+1, v)
				}
			}
			if strings.HasSuffix(name, "_btn") && (m.Path == nil || m.Path(shape) == "") {
				t.Errorf("%s: button template %q needs a link path", event, name)
			}
		}
	}
}

// maskaniBillShapes covers the optional parts of a bill: VAT, a fund without a paybill, and items.
func maskaniBillShapes(base map[string]any) []map[string]any {
	with := func(kv map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range kv {
			out[k] = v
		}
		return out
	}
	items := []any{
		map[string]any{"description": "Service charge", "amount": "4500.00"},
		map[string]any{"description": "Water", "amount": "1080.00", "quantity": "9", "rate": "120.00"},
		map[string]any{"description": "Gym", "amount": "1000.00", "tax": "160.00"},
	}
	untaxed := []any{items[0], items[1]}
	return []map[string]any{
		with(map[string]any{"items": untaxed, "subtotal": "5580.00", "tax_total": "0.00", "amount": "5580.00"}),
		with(map[string]any{"items": items, "subtotal": "6580.00", "tax_total": "160.00", "amount": "6740.00"}),
		with(map[string]any{"items": untaxed, "paybill": "", "subtotal": "5580.00", "tax_total": "0.00", "amount": "5580.00"}),
		with(map[string]any{"tax_total": "0.00"}), // no items on the event
	}
}

// TestMaskaniBillGatesOptionalLines: VAT appears only on a taxed bill, quantity and rate only where
// given, and the paybill line only when the fund has a paybill.
func TestMaskaniBillGatesOptionalLines(t *testing.T) {
	m := maskaniMappings["bill.issued"]
	ti := &tenantInfo{Name: "Shaba Village", Slug: "shaba-village"}
	shapes := maskaniBillShapes(map[string]any{"name": "Jane", "period": "2026-11", "account_ref": "B07",
		"due_date": "10 Nov 2026", "paybill": "4012345"})

	untaxed := m.Data(shapes[0], ti)
	if untaxed["has_tax"] == true {
		t.Error("untaxed bill reports VAT")
	}
	if name, params := m.WAVariant(untaxed); name != "maskani_bill_issued_v1_btn" || strings.Contains(strings.Join(params, "|"), "VAT") {
		t.Errorf("untaxed bill uses %q with %v", name, params)
	}
	if !strings.Contains(untaxed["charges"].(string), "Water 9 x KES 120 = KES 1,080") ||
		strings.Contains(untaxed["charges"].(string), "Service charge 1 x") {
		t.Errorf("charges line = %q", untaxed["charges"])
	}

	taxed := m.Data(shapes[1], ti)
	if name, _ := m.WAVariant(taxed); name != "maskani_bill_issued_vat_v1_btn" || taxed["has_tax"] != true {
		t.Errorf("taxed bill uses %q", name)
	}
	items := taxed["items"].([]maskaniBillItem)
	if items[0].Tax != "" || items[0].Detail != "" || items[2].Tax == "" {
		t.Errorf("per-line tax and detail gating wrong: %+v", items)
	}

	if name, _ := m.WAVariant(m.Data(shapes[2], ti)); name != "maskani_bill_issued_nopaybill_v1_btn" {
		t.Errorf("bill without paybill uses %q", name)
	}

	// A bill with many charges stays one bounded line.
	many := make([]any, 60)
	for i := range many {
		many[i] = map[string]any{"description": "Charge with a fairly long description", "amount": "100.00"}
	}
	if _, line := maskaniBillItems(many); len(line) > maxChargesParam+20 || !strings.Contains(line, "more") {
		t.Errorf("long charges line not cut: %d chars", len(line))
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
