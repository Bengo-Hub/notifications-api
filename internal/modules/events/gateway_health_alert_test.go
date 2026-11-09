package events

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

func renderEmail(t *testing.T, id string, data map[string]any) string {
	t.Helper()
	tpl, err := template.ParseFiles("../../../templates/email/" + id + ".html")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.ExecuteTemplate(&out, "content", data); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// The 9 November SasaPay maintenance, as ops and the affected tenant read it.
func TestGatewayAlertTemplatesRender(t *testing.T) {
	data := map[string]any{
		"provider": "payhero", "provider_label": providerLabel("payhero", "sasapay"), "upstream": "sasapay",
		"reason": "maintenance", "message": "Service currently unavailable", "since": readableTime("2026-11-09T14:54:11Z"),
		"tenant_name": "Hadia", "name": "Titus", "recovered_at": readableTime("2026-11-09T15:27:42Z"), "minutes": 34, "affected_count": 1,
	}
	cases := map[string][]string{
		gatewayDegradedTemplate:  {"M-Pesa payments are delayed", "sasapay", "in maintenance", "9 Nov 2026, 17:54 EAT", "nothing is lost", "Hadia"},
		gatewayRecoveredTemplate: {"working again", "about 34 minutes", "18:27 EAT"},
		paymentsDelayedTemplate:  {"Hello Titus", "because of maintenance", "Nothing is lost", "Please do not pay again"},
		paymentsRestoredTemplate: {"back to normal since 9 Nov 2026, 18:27 EAT"},
	}
	for id, wants := range cases {
		html := renderEmail(t, id, data)
		for _, want := range wants {
			if !strings.Contains(html, want) {
				t.Errorf("%s lacks %q", id, want)
			}
		}
	}
}

func TestProviderLabelAndList(t *testing.T) {
	if providerLabel("payhero", "") != "M-Pesa" || providerLabel("paystack", "") != "Paystack" || providerLabel("", "") != "The payment provider" {
		t.Fatal("labels")
	}
	if got := splitList(" +254700000001, ,+254700000002 "); len(got) != 2 {
		t.Fatalf("list %v", got)
	}
}
