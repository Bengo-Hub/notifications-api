package main

import (
	"bytes"
	"html/template"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

func renderRenewed(t *testing.T, payload map[string]any) string {
	t.Helper()
	m := subscriptionMappings["renewed"]
	tpl, err := template.ParseFiles(filepath.Join("..", "..", "templates", "email", filepath.FromSlash(m.TemplateID)+".html"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.ExecuteTemplate(&out, "content", m.DataBuilder(payload, "https://tenant.example")); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// A paid renewal is also the payment confirmation (10 November 2026: tenants got neither): it says
// what was paid and the receipt, and a renewal with no payment shows no payment lines.
func TestRenewedIsThePaymentConfirmation(t *testing.T) {
	paid := renderRenewed(t, map[string]any{
		"plan_name": "Duka Pro", "period_end": "2026-12-10T12:06:29Z", "amount_paid": "2500.00",
		"currency": "KES", "payment_method": "mpesa", "payment_reference": "TKA1B2C3D4",
	})
	for _, want := range []string{"Payment received", "Duka Pro", "10 Dec 2026", "KES 2500.00", "TKA1B2C3D4"} {
		if !strings.Contains(paid, want) {
			t.Fatalf("missing %q in:\n%s", want, paid)
		}
	}
	free := renderRenewed(t, map[string]any{"plan_name": "Duka Pro", "period_end": "2026-12-10T12:06:29Z"})
	if strings.Contains(free, "Amount paid") || strings.Contains(free, "Payment received") || strings.Contains(free, "<no value>") {
		t.Fatalf("a renewal with no payment shows payment lines:\n%s", free)
	}
}

// Billing notices about the tenant's own subscription default on: as optional they were never sent.
func TestSubscriptionNoticesAreEssential(t *testing.T) {
	for _, key := range []string{"subscription/subscription_renewed", "subscription/subscription_created",
		"subscription/subscription_upgraded", "subscription/subscription_downgraded", "subscription/subscription_cancelled"} {
		found := false
		for _, ty := range preferences.Registry {
			if ty.Key == key {
				found = true
				if ty.Class != preferences.ClassEssential {
					t.Errorf("%s is %s, want essential", key, ty.Class)
				}
			}
		}
		if !found {
			t.Errorf("%s not registered", key)
		}
	}
}
