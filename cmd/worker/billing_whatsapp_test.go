package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBooksButtonSuffix(t *testing.T) {
	cases := []struct {
		links []string
		want  string
	}{
		{[]string{"https://books.codevertexafrica.com/i/abc-123", "https://books.codevertexafrica.com/pay?tenant=x"}, "i/abc-123"},
		{[]string{"", "https://books.codevertexafrica.com/pay?tenant=x&amount=3000.00"}, "pay?tenant=x&amount=3000.00"},
		{[]string{"https://evil.example/i/abc"}, ""},
		{[]string{"", ""}, ""},
	}
	for _, c := range cases {
		if got := booksButtonSuffix(c.links...); got != c.want {
			t.Errorf("booksButtonSuffix(%v) = %q, want %q", c.links, got, c.want)
		}
	}
}

func TestBillingWhatsAppParamsMatchTemplates(t *testing.T) {
	raw, err := os.ReadFile("../../internal/whatsapp/templatesync/templates.json")
	if err != nil {
		t.Fatal(err)
	}
	var defs []struct {
		Name    string   `json:"name"`
		Example []string `json:"example"`
		Buttons []struct {
			URL string `json:"url"`
		} `json:"buttons"`
	}
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatal(err)
	}
	byName := map[string]int{}
	for _, d := range defs {
		byName[d.Name] = len(d.Example)
		if strings.HasPrefix(d.Name, "subscription_") {
			if len(d.Buttons) != 1 || !strings.HasPrefix(d.Buttons[0].URL, sharedBooksBase+"/") {
				t.Errorf("%s: button must point at %s", d.Name, sharedBooksBase)
			}
		}
	}

	ti := &tenantInfo{Name: "Alpha China Market"}
	invoice := map[string]any{"amount": 3000.0, "currency": "KES", "invoice_number": "INV-1", "due_date": "2026-10-08T00:00:00Z"}
	grace := map[string]any{"amount": "KES 3000.00", "invoice_number": "INV-1", "days_remaining": 3}
	for evt, spec := range billingWhatsAppEvents {
		payload := invoice
		if spec.reminder {
			payload = grace
		}
		params := billingWhatsAppParams(spec, ti, payload)
		want, ok := byName[spec.template]
		if !ok {
			t.Errorf("%s: template %s missing from templates.json", evt, spec.template)
			continue
		}
		if len(params) != want {
			t.Errorf("%s: %d params, template %s takes %d", evt, len(params), spec.template, want)
		}
		if _, err := os.Stat("../../templates/whatsapp/" + spec.textID + ".txt"); err != nil {
			t.Errorf("%s: text body %s missing", evt, spec.textID)
		}
	}
	if p := billingWhatsAppParams(billingWhatsAppEvents["invoice_generated"], ti, invoice); p[3] != "KES 3,000" || p[4] != "08 Oct 2026" {
		t.Errorf("invoice params = %v", p)
	}
	if p := billingWhatsAppParams(billingWhatsAppEvents["grace_reminder"], ti, grace); p[2] != "KES 3000.00" || p[4] != "3" {
		t.Errorf("grace params = %v", p)
	}
}
