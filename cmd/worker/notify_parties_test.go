package main

import (
	"bytes"
	htmltemplate "html/template"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// partyContacts applies the shared channel rules: placeholder emails are skipped, a button
// template never goes without its link, SMS and push only when the party asks for them.
func TestPartyContacts(t *testing.T) {
	wa := &partyWhatsApp{Name: "x_v1_btn", Params: []string{"a"}, Button: "slug/page"}
	got := partyContacts(noticeParty{Email: "a@b.co", Phone: "0711", WhatsApp: wa})
	if got["email"] != "a@b.co" || got["whatsapp"] != "0711" || got["sms"] != "" {
		t.Fatalf("default channels wrong: %v", got)
	}
	if got := partyContacts(noticeParty{Email: "p254@placeholder.local", Phone: "0711", WhatsApp: &partyWhatsApp{Name: "x_v1_btn"}}); len(got) != 0 {
		t.Fatalf("placeholder email or linkless button template sent: %v", got)
	}
	if got := partyContacts(noticeParty{Phone: "0711", WhatsApp: &partyWhatsApp{Name: "plain_v1"}}); got["whatsapp"] != "0711" {
		t.Fatalf("a non-button template needs no link: %v", got)
	}
	got = partyContacts(noticeParty{Email: "a@b.co", Phone: "0711", PushTo: "u1", Channels: []string{"email", "sms", "whatsapp", "push"}})
	if got["sms"] != "0711" || got["push"] != "u1" || got["whatsapp"] != "" {
		t.Fatalf("explicit channels wrong (no WhatsApp template means no WhatsApp): %v", got)
	}
}

// Every insurance WhatsApp template is in the manifest with the parameter count the code sends,
// has a body file, and gets single-line non-empty parameters; both emails render for both
// audiences.
func TestVehicleInsuranceTemplates(t *testing.T) {
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatal(err)
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
	payload := map[string]any{
		"license_plate": "KMGX040M", "policy_no": "030/071/1/000255/2026/03", "insurer": "Old Mutual",
		"expires_on": "2027-03-24", "days_left": float64(6), "blocked": true, "tenant_slug": "urban-loft",
		"rider_name": "Phelix Omundo", "rider_email": "rider@example.com", "rider_phone": "0793487529",
	}
	data := insuranceData(payload, &tenantInfo{Name: "The Urban Loft", Slug: "urban-loft"})
	if data["expires_on"] != "24 Mar 2027" || data["name"] != "Phelix" {
		t.Fatalf("data wrong: %v", data)
	}
	root := filepath.Join("..", "..", "templates")
	for _, expired := range []bool{false, true} {
		event := "expiring"
		if expired {
			event = "expired"
		}
		for _, audience := range []string{"rider", "business"} {
			name, got := insuranceWhatsApp(expired, audience, data)
			if want, ok := params[name]; !ok || want != len(got) {
				t.Errorf("%s: manifest has %d params (present %v), code sends %d", name, want, ok, len(got))
			}
			for i, v := range got {
				if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\n\t") {
					t.Errorf("%s: param %d empty or multi-line: %q", name, i+1, v)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "whatsapp", "logistics", "vehicle_insurance_"+event+"_"+audience+".txt")); err != nil {
				t.Errorf("missing whatsapp body for %s %s", event, audience)
			}
			tpl, err := htmltemplate.ParseFiles(filepath.Join(root, "email", "logistics", "vehicle_insurance_"+event+".html"))
			if err != nil {
				t.Fatal(err)
			}
			d := map[string]any{}
			for k, v := range data {
				d[k] = v
			}
			d["audience"] = audience
			d["action_link"] = "https://app.example.com/urban-loft/x"
			var out bytes.Buffer
			if err := tpl.ExecuteTemplate(&out, "content", d); err != nil {
				t.Fatalf("%s %s: %v", event, audience, err)
			}
			html := out.String()
			if !strings.Contains(html, "KMGX040M") || !strings.Contains(html, "24 Mar 2027") {
				t.Errorf("%s %s email lacks plate or date", event, audience)
			}
			if audience == "business" && !strings.Contains(html, "Phelix Omundo") {
				t.Errorf("%s business email lacks the rider", event)
			}
		}
	}
	// No rider on the vehicle: the business message still has a rider label.
	noRider := insuranceData(map[string]any{"license_plate": "KX1", "expires_on": "2027-03-24"}, &tenantInfo{Name: "B"})
	if noRider["rider_name"] != "no rider linked" || noRider["name"] != "there" {
		t.Fatalf("no rider labels wrong: %v", noRider)
	}
}
