package main

import (
	"bytes"
	"html/template"
	"path/filepath"
	"strings"
	"testing"
)

// The 12 November 2026 template audit found emails that rendered blank text because a builder set
// a key the template does not read, or read a payload key the publisher never sends. Each case
// renders the real template with the real builder and the publisher's real payload.

func renderEmail(t *testing.T, templateID string, data map[string]interface{}) string {
	t.Helper()
	tpl, err := template.ParseFiles(filepath.Join("..", "..", "templates", "email", filepath.FromSlash(templateID)+".html"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.ExecuteTemplate(&out, "content", data); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func mustContain(t *testing.T, name, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Fatalf("%s: missing %q in:\n%s", name, w, out)
		}
	}
}

func TestMilestoneEmailShowsProgress(t *testing.T) {
	m := projectsMappings["milestone.reached"]
	out := renderEmail(t, m.TemplateID, m.DataBuilder(map[string]any{
		"project_name": "Fit-out", "milestone_name": "Handover", "completion_date": "2026-11-12T09:00:00Z", "progress": 100,
	}, "https://t.example"))
	mustContain(t, "milestone", out, "100%", "12 Nov 2026")
}

func TestPayoutAndRefundEmailsHaveNoBlanks(t *testing.T) {
	p := treasuryMappings["payout.completed"]
	out := renderEmail(t, p.TemplateID, p.DataBuilder(map[string]any{
		"net_amount": "9000.00", "currency": "KES", "reference": "PO-1", "period_end": "2026-11-11T21:00:00Z",
	}, "https://t.example"))
	mustContain(t, "payout", out, "Date:</strong> 11 Nov 2026")
	r := treasuryMappings["refund.completed"]
	out = renderEmail(t, r.TemplateID, r.DataBuilder(map[string]any{"amount": "500", "currency": "KES"}, "https://t.example"))
	if strings.Contains(out, "Hello ,") {
		t.Fatalf("refund greeting is blank:\n%s", out)
	}
}

func TestStockAlertsNameTheWarehouseAndTime(t *testing.T) {
	payload := map[string]interface{}{"name": "Sugar 1kg", "sku": "SUG1", "available": 0.0, "reorder_level": 5,
		"warehouse_id": "0b6f8d9e-1111-2222-3333-444455556666", "warehouse_name": "Main Store", "detected_at": "2026-11-12T09:00:00Z"}
	for _, ev := range []string{"inventory.stock.low", "inventory.stock.out"} {
		m := inventoryMappings[ev]
		out := renderEmail(t, m.TemplateID, m.DataBuilder(payload, "https://t.example"))
		mustContain(t, ev, out, "Sugar 1kg", "Main Store")
		if strings.Contains(out, "0b6f8d9e") {
			t.Fatalf("%s shows the warehouse UUID:\n%s", ev, out)
		}
	}
	m := inventoryMappings["inventory.stock.out"]
	mustContain(t, "stock.out time", renderEmail(t, m.TemplateID, m.DataBuilder(payload, "https://t.example")), "12 Nov 2026")
	if _, dup := inventoryMappings["inventory.ticket.issued"]; dup {
		t.Fatal("ticket.issued is also sent by the cross-service subscriber; the worker copy must stay removed")
	}
}

func TestScheduledOrderTimeIsReadable(t *testing.T) {
	if got := formatEventTime("2026-11-12T15:00:00Z"); got != "12 Nov 2026, 6:00 PM" {
		t.Fatalf("got %q", got)
	}
	if got := formatEventTime("not a time"); got != "not a time" {
		t.Fatalf("got %q", got)
	}
}
