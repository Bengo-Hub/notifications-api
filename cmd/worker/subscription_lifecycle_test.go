package main

import (
	"bytes"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

func renderSubscription(t *testing.T, event string, payload map[string]any) string {
	t.Helper()
	m, ok := subscriptionMappings[event]
	if !ok {
		t.Fatalf("%s is not mapped", event)
	}
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

// 12 November 2026: Gram Auto Spares got "Your subscription for has reached the end of its billing
// period" with an empty Plan line, because grace events carried no plan and the consumer read only
// plan_code. The plan name now comes through, and an event without one leaves no broken sentence.
func TestGraceEmailsNameThePlan(t *testing.T) {
	grace := map[string]any{"plan_code": "DUKA_BASIC", "plan_name": "Duka Basic", "days_remaining": 7,
		"grace_ends_at": "19 Nov 2026", "amount": "KES 3000.00", "invoice_number": "INV-261105-000037", "pay_link": "https://pay.example/x"}
	for _, event := range []string{"grace_started", "grace_reminder", "trial_ended"} {
		out := renderSubscription(t, event, grace)
		for _, want := range []string{"Duka Basic", "INV-261105-000037", "KES 3000.00", "19 Nov 2026"} {
			if !strings.Contains(out, want) {
				t.Fatalf("%s: missing %q in:\n%s", event, want, out)
			}
		}
	}
	bare := renderSubscription(t, "grace_reminder", map[string]any{"days_remaining": 7, "grace_ends_at": "19 Nov 2026", "amount": "KES 3000.00"})
	if strings.Contains(bare, "for  has") || strings.Contains(bare, "for <strong></strong>") || strings.Contains(bare, "Plan:") {
		t.Fatalf("an event with no plan leaves a broken line:\n%s", bare)
	}
}

func TestCreatedAndCancelledNameThePlan(t *testing.T) {
	if out := renderSubscription(t, "created", map[string]any{"plan_code": "DUKA_BASIC", "plan_name": "Duka Basic"}); !strings.Contains(out, "Duka Basic") {
		t.Fatalf("created:\n%s", out)
	}
	out := renderSubscription(t, "cancelled", map[string]any{"plan_code": "DUKA_BASIC", "plan_name": "Duka Basic", "access_until": "05 Dec 2026"})
	for _, want := range []string{"Duka Basic", "05 Dec 2026"} {
		if !strings.Contains(out, want) {
			t.Fatalf("cancelled: missing %q in:\n%s", want, out)
		}
	}
}

// Blocking access, a failed payment and dormancy used to be dropped silently: no mapping, no email.
func TestBlockingAndDormancyEventsAreSent(t *testing.T) {
	expired := renderSubscription(t, "expired", map[string]any{"plan_name": "Duka Basic", "blocked_on": "19 Nov 2026",
		"amount": "KES 3000.00", "invoice_number": "INV-261105-000037", "pay_link": "https://pay.example/x"})
	for _, want := range []string{"access is blocked", "Duka Basic", "INV-261105-000037", "https://pay.example/x"} {
		if !strings.Contains(expired, want) {
			t.Fatalf("expired: missing %q in:\n%s", want, expired)
		}
	}
	warn := renderSubscription(t, "dormancy.warning", map[string]any{"grace_ends_at": "2026-11-19T00:00:00Z",
		"notification": map[string]any{"message": "Reactivate within 7 days."}})
	for _, want := range []string{"inactive", "Reactivate within 7 days.", "19 Nov 2026", "/settings/subscription"} {
		if !strings.Contains(warn, want) {
			t.Fatalf("dormancy.warning: missing %q in:\n%s", want, warn)
		}
	}
	susp := renderSubscription(t, "dormancy.suspended", map[string]any{"notification": map[string]any{"message": "Contact support."}})
	if !strings.Contains(susp, "mailto:support@codevertexafrica.com") || strings.Contains(susp, "Act by") {
		t.Fatalf("dormancy.suspended:\n%s", susp)
	}
	if out := renderSubscription(t, "payment_required", map[string]any{}); !strings.Contains(out, "suspended") {
		t.Fatalf("payment_required:\n%s", out)
	}
}

// Every mapped subscription event has its email template on disk and a registered, essential
// preference, so no billing notice is dropped or switched off by default.
func TestEverySubscriptionMappingHasATemplate(t *testing.T) {
	for event, m := range subscriptionMappings {
		if _, err := os.Stat(filepath.Join("..", "..", "templates", "email", filepath.FromSlash(m.TemplateID)+".html")); err != nil {
			t.Errorf("%s: template %s missing", event, m.TemplateID)
		}
		registered := false
		for _, ty := range preferences.Registry {
			if ty.Key == m.TemplateID {
				registered = ty.Class == preferences.ClassEssential
			}
		}
		if !registered && !strings.HasPrefix(m.TemplateID, "finance/") {
			t.Errorf("%s: %s is not registered as essential", event, m.TemplateID)
		}
	}
}
