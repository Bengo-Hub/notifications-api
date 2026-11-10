package events

import (
	"bytes"
	"html/template"
	"path/filepath"
	"strings"
	"testing"
)

// Both purchase order received emails exist and render the data handlePOReceived sends (10 November
// 2026 the worker logged "template not found: email/inventory/po_received").
func TestPOReceivedTemplatesRender(t *testing.T) {
	data := map[string]any{"po_number": "PO-261110-001452", "supplier_name": "Acme Supplies", "total_amount": 12500.5, "currency": "KES"}
	for _, id := range []string{"inventory/po_received", "inventory/po_vendor_confirmation"} {
		tpl, err := template.ParseFiles(filepath.Join("..", "..", "..", "templates", "email", filepath.FromSlash(id)+".html"))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		var out bytes.Buffer
		if err := tpl.ExecuteTemplate(&out, "content", data); err != nil {
			t.Fatalf("%s render: %v", id, err)
		}
		s := out.String()
		if !strings.Contains(s, "PO-261110-001452") || !strings.Contains(s, "KES 12500.50") || strings.Contains(s, "<no value>") {
			t.Fatalf("%s:\n%s", id, s)
		}
	}
}
