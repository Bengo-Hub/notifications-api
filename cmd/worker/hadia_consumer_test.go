package main

import (
	"encoding/json"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Buttons open hadiafrica.com only; a link on any other host gives no button.
// FR-41: a link check notice goes to the owner's email only and says which way the link changed.
func TestHadiaItemLinkChanged(t *testing.T) {
	msgs := hadiaMappings["item.link_changed"]
	if len(msgs) != 1 || msgs[0].PhoneKey != "" || msgs[0].EmailKey != "owner_email" {
		t.Fatalf("mapping: %+v", msgs)
	}
	m := msgs[0]
	changed := map[string]any{"owner_name": "Amina", "item_title": "Kettle", "registry_title": "Wedding", "store_name": "Jumia",
		"link_status": "changed", "price_seen": "2500.00", "price_now": "2899.00", "owner_email": "a@example.com"}
	d := m.Data(changed, &tenantInfo{})
	if d["gone"] != false || d["price_now"] == "" || d["store"] != "Jumia" {
		t.Fatalf("changed data: %+v", d)
	}
	if !strings.Contains(m.Subject(changed), "price") {
		t.Fatalf("subject: %s", m.Subject(changed))
	}
	gone := m.Data(map[string]any{"link_status": "unavailable", "owner_email": "a@example.com"}, &tenantInfo{})
	if gone["gone"] != true || gone["store"] != "the shop" {
		t.Fatalf("gone data: %+v", gone)
	}
	if !m.Skip(map[string]any{"owner_phone": "+254700000000"}) {
		t.Fatal("a notice with no email must be skipped")
	}
	tpl, err := template.ParseFiles("../../templates/email/shared/base.html", "../../templates/email/hadia/item_link_changed.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		data map[string]any
		want string
	}{{d, "now shows"}, {gone, "not there any more"}} {
		var out strings.Builder
		if err := tpl.ExecuteTemplate(&out, "base.html", c.data); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.want) {
			t.Fatalf("%q did not render", c.want)
		}
	}
}

// Every Hadia email names the real address, through the shared footer (Sprint 08, look-alike sites).
func TestHadiaTrustNoteInFooter(t *testing.T) {
	if !strings.Contains(hadiaTrustNote, "hadiafrica.com") {
		t.Fatalf("note does not name the domain: %q", hadiaTrustNote)
	}
	if got := (giftDigest{}).data()["brand_note"]; got != hadiaTrustNote {
		t.Fatalf("digest brand_note = %v", got)
	}
	tpl, err := template.ParseFiles("../../templates/email/shared/base.html", "../../templates/email/hadia/gift_receipt.html")
	if err != nil {
		t.Fatal(err)
	}
	var withNote, without strings.Builder
	if err := tpl.ExecuteTemplate(&withNote, "base.html", map[string]any{"brand_note": hadiaTrustNote}); err != nil {
		t.Fatal(err)
	}
	if err := tpl.ExecuteTemplate(&without, "base.html", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withNote.String(), "only ever use hadiafrica.com") {
		t.Fatal("the footer does not show the note")
	}
	if strings.Contains(without.String(), "hadiafrica.com") {
		t.Fatal("other tenants' emails must not carry Hadia's note")
	}
}

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
			if m.PhoneKey == "" {
				continue // an email-only message (staff.invited) has no WhatsApp template
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

// A guest release tells the owner, with the guest's note, by email; an expiry or an owner's own
// release does not (owner, 9 November 2026). The reminder's subject says when it is the last day.
func TestHadiaReservationReleasedOwner(t *testing.T) {
	msgs := hadiaMappings["reservation.released"]
	if len(msgs) != 2 || msgs[1].TemplateID != "hadia/reservation_released_owner" || msgs[1].PhoneKey != "" {
		t.Fatalf("mapping: %+v", msgs)
	}
	owner := msgs[1]
	p := map[string]any{"reason": "cancelled", "owner_email": "o@example.com", "owner_name": "Trae", "guest_name": "Juma",
		"show_reserver_names": false, "item_title": "Dinner set", "registry_title": "House warming", "note": "Sorry, budget changed"}
	if owner.Skip(p) {
		t.Fatal("a guest release was skipped")
	}
	d := owner.Data(p, &tenantInfo{})
	if d["guest"] != "A guest" || d["note"] != "Sorry, budget changed" {
		t.Fatalf("data: %+v", d)
	}
	for _, reason := range []string{"expired", "owner"} {
		p["reason"] = reason
		if !owner.Skip(p) {
			t.Fatalf("%s release reached the owner", reason)
		}
	}
	tpl, err := template.ParseFiles(filepath.Join("..", "..", "templates", "email", "hadia", "reservation_released_owner.html"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tpl.ExecuteTemplate(&b, "content", d); err != nil || !strings.Contains(b.String(), "Sorry, budget changed") {
		t.Fatalf("render: %v %s", err, b.String())
	}
	rem := hadiaMappings["reservation.reminder"][0]
	if !strings.Contains(rem.Subject(map[string]any{"last_day": true}), "Last day") || strings.Contains(rem.Subject(map[string]any{}), "Last day") {
		t.Fatal("reminder subject")
	}
}
