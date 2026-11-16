package events

import (
	"strings"
	"testing"
)

// A move to Marketing is an emailed alert that says what it costs, what Meta does to delivery and
// what to do; a cheaper move or an approval is Slack-only news.
func TestWhatsAppTemplateAlertWording(t *testing.T) {
	subject, text, data, email := whatsAppTemplateAlert(WhatsAppTemplateChange{
		Kind: "category", WABAID: "W1", Name: "credits_balance_low_v1_btn", PreviousCategory: "UTILITY", NewCategory: "MARKETING", Source: "webhook",
	})
	if !email || !strings.Contains(subject, "Marketing") || !strings.Contains(text, "131049") ||
		!strings.Contains(text, "credits_balance_low_v1_btn") || !strings.Contains(data["action"].(string), "superseded.go") {
		t.Fatalf("to marketing: %q %q %v %v", subject, text, data, email)
	}

	_, text, _, email = whatsAppTemplateAlert(WhatsAppTemplateChange{Kind: "category", Name: "x", PreviousCategory: "MARKETING", NewCategory: "UTILITY"})
	if email || !strings.Contains(text, "Utility") {
		t.Fatalf("to utility: %q %v", text, email)
	}

	_, _, _, email = whatsAppTemplateAlert(WhatsAppTemplateChange{Kind: "status", Name: "x", Status: "APPROVED"})
	if email {
		t.Fatal("an approval was emailed")
	}

	subject, text, _, email = whatsAppTemplateAlert(WhatsAppTemplateChange{Kind: "status", Name: "x", Status: "REJECTED", Reason: "INCORRECT_CATEGORY"})
	if !email || !strings.Contains(subject, "rejected") || !strings.Contains(text, "INCORRECT_CATEGORY") {
		t.Fatalf("rejected: %q %q %v", subject, text, email)
	}
}
