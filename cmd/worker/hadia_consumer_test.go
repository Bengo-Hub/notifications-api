package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Buttons open hadiafrica.com only; a link on any other host gives no button.
func TestHadiaPath(t *testing.T) {
	for in, want := range map[string]string{
		"https://hadiafrica.com/r/AB12CD34":                 "r/AB12CD34",
		"https://www.hadiafrica.com/r/AB12CD34/reserved/tk": "r/AB12CD34/reserved/tk",
		"https://hadiafrica.com/app/registries/42/":         "app/registries/42",
		"https://evil.example/r/AB12CD34":                   "",
		"https://hadiafrica.com.evil.example/r/x":           "",
		"r/AB12CD34": "",
		"":           "",
	} {
		if got := hadiaPath(in); got != want {
			t.Errorf("hadiaPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every mapped message has its email and WhatsApp template files, every WhatsApp template it
// names is declared for Meta in templates.json, and a _btn template has a link to fill.
func TestHadiaMappingsHaveTemplates(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "whatsapp", "templatesync", "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var declared []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &declared); err != nil {
		t.Fatal(err)
	}
	meta := map[string]bool{}
	for _, d := range declared {
		meta[d.Name] = true
	}
	for event, msgs := range hadiaMappings {
		for _, m := range msgs {
			if _, err := os.Stat(filepath.Join("..", "..", "templates", "email", m.TemplateID+".html")); err != nil {
				t.Errorf("%s: email template %s missing", event, m.TemplateID)
			}
			wa := m.TemplateID
			if m.WATemplateID != "" {
				wa = m.WATemplateID
			}
			if _, err := os.Stat(filepath.Join("..", "..", "templates", "whatsapp", wa+".txt")); err != nil {
				t.Errorf("%s: whatsapp template %s missing", event, wa)
			}
			if m.WAVariant != nil {
				continue // TestHadiaPayoutCompletedShort checks the variants
			}
			if !meta[m.WATemplate] {
				t.Errorf("%s: %s is not declared in templates.json", event, m.WATemplate)
			}
			if strings.HasSuffix(m.WATemplate, "_btn") && m.PathKey == "" && m.FixedPath == "" {
				t.Errorf("%s: %s needs a PathKey or a FixedPath for its button", event, m.WATemplate)
			}
		}
	}
}

// A split payout that stopped early says what went back, in a template of its own.
func TestHadiaPayoutCompletedShort(t *testing.T) {
	m := hadiaMappings["payout.completed"][0]
	base := map[string]any{"owner_name": "Amina", "registry_title": "Wedding", "gross": "150000.00", "net": "150000.00", "currency": "KES", "reference": "ESC-A-1"}
	if name, params := m.WAVariant(m.Data(base, &tenantInfo{})); name != "hadia_payout_completed_v1_btn" || len(params) != 4 {
		t.Fatalf("%s %v", name, params)
	}
	base["returned"] = "300000.00"
	name, params := m.WAVariant(m.Data(base, &tenantInfo{}))
	if name != "hadia_payout_completed_part_v1_btn" || len(params) != 5 {
		t.Fatalf("%s %v", name, params)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "whatsapp", "templatesync", "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"hadia_payout_completed_part_v1_btn"`) || !strings.Contains(string(raw), `"hadia_payout_completed_v1_btn"`) {
		t.Error("a payout variant is not declared in templates.json")
	}
}

// A new case goes to the tenant's contact, on WhatsApp only when it is high or urgent, and the
// summary stays out of WhatsApp.
func TestHadiaCaseOpened(t *testing.T) {
	m := hadiaMappings["case.opened"][0]
	if !m.Staff || m.FixedPath != "admin/exceptions" {
		t.Fatalf("staff %v, path %q", m.Staff, m.FixedPath)
	}
	for priority, want := range map[string]bool{"urgent": true, "high": true, "normal": false, "low": false, "": false} {
		if got := m.WhatsAppIf(map[string]any{"priority": priority}); got != want {
			t.Errorf("priority %q: WhatsApp %v", priority, got)
		}
	}
	d := m.Data(map[string]any{"priority": "urgent", "kind": "manual_refund", "summary": "A card gift was charged back", "case_id": "c1"}, &tenantInfo{})
	if d["kind"] != "manual refund" || d["summary"] != "A card gift was charged back" {
		t.Fatalf("data %v", d)
	}
	params := m.WAParams(d)
	if strings.Contains(strings.Join(params, " "), "charged back") {
		t.Errorf("the summary reached WhatsApp: %v", params)
	}
	if subject := m.Subject(map[string]any{"priority": "urgent", "kind": "manual_refund"}); !strings.Contains(subject, "manual refund") {
		t.Errorf("subject %q", subject)
	}
}

// A payout number change alerts the OLD number (and the email), never the new one.
func TestHadiaPayoutChangeAlertsOldNumber(t *testing.T) {
	m := hadiaMappings["payout_destination.changed"][0]
	if m.PhoneKey != "old_phone" || m.EmailKey != "owner_email" {
		t.Fatalf("alert goes to %s and %s, want old_phone and owner_email", m.PhoneKey, m.EmailKey)
	}
	d := m.Data(map[string]any{"owner_name": "Amina", "old_phone_masked": "+254 7** *** 901", "new_phone_masked": "+254 7** *** 345"}, &tenantInfo{})
	if d["old_number"] != "+254 7** *** 901" || d["new_number"] != "+254 7** *** 345" {
		t.Errorf("data %v", d)
	}
}

// The owner sees the guest's name only when the registry shows reserver names (FR-15).
func TestHadiaOwnerAlertHidesGuestName(t *testing.T) {
	alert := hadiaMappings["reservation.created"][1]
	hidden := alert.Data(map[string]any{"guest_name": "Wanjiru", "item_title": "Kettle"}, &tenantInfo{})
	if hidden["guest"] != "A guest" {
		t.Errorf("hidden names: guest = %v", hidden["guest"])
	}
	shown := alert.Data(map[string]any{"guest_name": "Wanjiru", "item_title": "Kettle", "show_reserver_names": true}, &tenantInfo{})
	if shown["guest"] != "Wanjiru" {
		t.Errorf("shown names: guest = %v", shown["guest"])
	}
}

// The guest receipt goes only to a known guest phone and shows gift, fee and total (T5).
func TestHadiaGiftReceipt(t *testing.T) {
	receipt := hadiaMappings["gift.received"][0]
	if !receipt.Skip(map[string]any{"guest_name": "Jane"}) {
		t.Error("a gift with no guest phone must send no receipt")
	}
	d := receipt.Data(map[string]any{"guest_name": "Jane", "registry_title": "Wedding", "amount": "5000.00", "fee": "52.00", "total": "5052.00", "receipt_ref": "TK41AB2CDE"}, &tenantInfo{})
	if d["fee"] != "KES 52" || d["total"] != "KES 5,052" || d["receipt"] != "TK41AB2CDE" {
		t.Errorf("receipt data %v", d)
	}
	// Before treasury prices the fee the payload may carry none: the total is the gift.
	d = receipt.Data(map[string]any{"amount": "5000.00"}, &tenantInfo{})
	if d["fee"] != "KES 0" || d["total"] != "KES 5,000" {
		t.Errorf("no-fee receipt data %v", d)
	}
}

// Exactly one message per reason, and an unconfirmed payout never claims the money is safe.
func TestHadiaPayoutFailureMessages(t *testing.T) {
	msgs := hadiaMappings["payout.delayed"]
	for reason, want := range map[string]string{
		"insufficient wallet balance": "hadia/payout_delayed",
		"":                            "hadia/payout_delayed",
		"approval_rejected":           "hadia/payout_declined",
		"payout_refused":              "hadia/payout_declined",
		"not_confirmed":               "hadia/payout_unconfirmed",
	} {
		var sent []string
		for _, m := range msgs {
			if !m.Skip(map[string]any{"reason": reason}) {
				sent = append(sent, m.TemplateID)
			}
		}
		if len(sent) != 1 || sent[0] != want {
			t.Errorf("reason %q sent %v, want only %s", reason, sent, want)
		}
	}
	for _, name := range []string{"email/hadia/payout_unconfirmed.html", "whatsapp/hadia/payout_unconfirmed.txt"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "templates", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if lower := strings.ToLower(string(raw)); strings.Contains(lower, "safe") || strings.Contains(lower, "back in your registry") {
			t.Errorf("%s must not say the money is safe", name)
		}
	}
	// A failure before treasury answered has no net: the gross is shown, not KES 0.
	d := msgs[1].Data(map[string]any{"net": "0.00", "gross": "5000.00", "registry_title": "Wedding"}, &tenantInfo{})
	if d["amount"] != "KES 5,000" {
		t.Errorf("amount %v", d["amount"])
	}
}
