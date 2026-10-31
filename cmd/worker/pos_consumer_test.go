package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// Every POS notice reaches its customer by email or WhatsApp: both template files exist, the
// WhatsApp template is in the manifest with exactly as many parameters as the notice sends, and
// no parameter is empty (Meta refuses the send).
func TestPosNoticesMatchTemplatesAndManifest(t *testing.T) {
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	placeholder := regexp.MustCompile(`\{\{(\d+)\}\}`)
	bodyParams := map[string]int{}
	for _, d := range defs {
		seen := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(d.Body, -1) {
			seen[m[1]] = true
		}
		bodyParams[d.Name] = len(seen)
	}
	// A payload with every field empty except the contacts, so fallbacks are exercised.
	ti := &tenantInfo{Name: "Urban Loft Cafe", Country: "KE"}
	for event, n := range posNotices {
		for _, path := range []string{"email/" + n.TemplateID + ".html", "whatsapp/" + n.TemplateID + ".txt"} {
			if _, err := os.Stat(filepath.Join("..", "..", "templates", path)); err != nil {
				t.Errorf("%s: missing template %s", event, path)
			}
		}
		evt := posEvent{ID: "e1", TenantID: "t1", EventType: event, Payload: map[string]any{
			n.EmailKey: "jane@example.com", n.PhoneKey: "0712345678",
		}}
		notice, ok := posPartyNotice(evt, ti)
		if !ok {
			t.Errorf("%s: no notice built for a customer with contacts", event)
			continue
		}
		wa := notice.Parties[0].WhatsApp
		want, inManifest := bodyParams[wa.Name]
		if !inManifest {
			t.Errorf("%s: WhatsApp template %q is not in templates.json", event, wa.Name)
			continue
		}
		if len(wa.Params) != want {
			t.Errorf("%s: %q expects %d parameters, the notice sends %d", event, wa.Name, want, len(wa.Params))
		}
		for i, p := range wa.Params {
			if p == "" {
				t.Errorf("%s: parameter {{%d}} is empty", event, i+1)
			}
		}
		if got := partyContacts(notice.Parties[0]); got["email"] == "" || got["whatsapp"] == "" || got["sms"] != "" {
			t.Errorf("%s: channels %v, want email and WhatsApp only", event, got)
		}
	}
}

func TestPosNoticeNeedsACustomerContact(t *testing.T) {
	if _, ok := posPartyNotice(posEvent{EventType: "loyalty.points.earned", Payload: map[string]any{"customer_name": "Jane"}}, nil); ok {
		t.Fatal("a notice without email or phone must not be sent")
	}
	if _, ok := posPartyNotice(posEvent{EventType: "kds.waiter.called", Payload: map[string]any{"customer_phone": "0712"}}, nil); ok {
		t.Fatal("removed POS events must not be handled")
	}
}

func TestPosPayloadHelpers(t *testing.T) {
	p := map[string]any{"amount": "1500.50", "n": float64(3), "d": "2026-11-02", "ts": "2026-11-02T10:00:00Z"}
	if payNum(p, "amount") != 1500.5 || payInt(p, "n") != 3 {
		t.Fatal("numbers from strings and JSON numbers")
	}
	if payDate(p, "d") != "2 Nov 2026" || payDate(p, "ts") != "2 Nov 2026" {
		t.Fatalf("dates: %q %q", payDate(p, "d"), payDate(p, "ts"))
	}
	if humanize("gold_plus") != "Gold plus" || nonEmptyOr(" ", "x") != "x" {
		t.Fatal("humanize and nonEmptyOr")
	}
}
