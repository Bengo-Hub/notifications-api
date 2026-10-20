package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func autoTopupEvent() subscriptionEvent {
	return subscriptionEvent{
		ID: "evt-1", TenantID: "t-1", AggregateID: "sub-1", EventType: eventAutoTopupFailed,
		Payload: map[string]any{"service_label": "SMS", "units": 500.0, "amount": 500.0, "reason": "the card was declined"},
	}
}

func TestAutoTopupFailedNoticeGoesByEmailAndWhatsAppWithTheBillingButton(t *testing.T) {
	ti := &tenantInfo{Name: "Alpha China Market", Country: "KE"}
	n := autoTopupFailedNotice(autoTopupEvent(), "t-1", ti, "admin@alpha.example", []string{"0712345678", "0799999999"})
	if len(n.Parties) != 1 {
		t.Fatalf("one recipient, got %d", len(n.Parties))
	}
	contacts := partyContacts(n.Parties[0])
	if contacts["email"] != "admin@alpha.example" || contacts["whatsapp"] != "0712345678" {
		t.Fatalf("both channels to the first billing contact: %v", contacts)
	}
	wa := n.Parties[0].WhatsApp
	if wa.Name != "credits_auto_topup_failed_v1_btn" || wa.Button != "billing" {
		t.Fatalf("whatsapp: %+v", wa)
	}
	if n.Scope != "platform" || n.IdemBase != "credits-auto-topup-failed-evt-1" {
		t.Fatalf("platform sender and a stable key per event: scope=%q key=%q", n.Scope, n.IdemBase)
	}
	if n.Data["action_link"] != pricingAppBase+"/billing" {
		t.Fatalf("link: %v", n.Data["action_link"])
	}
}

func TestAutoTopupFailedNoticeWithoutContactsHasNoChannels(t *testing.T) {
	n := autoTopupFailedNotice(autoTopupEvent(), "t-1", &tenantInfo{Name: "X"}, "", nil)
	if len(partyContacts(n.Parties[0])) != 0 {
		t.Fatal("nobody to tell must produce no channel")
	}
	onlyPhone := autoTopupFailedNotice(autoTopupEvent(), "t-1", &tenantInfo{Name: "X"}, "bad@host.local", []string{"0712345678"})
	if c := partyContacts(onlyPhone.Parties[0]); c["email"] != "" || c["whatsapp"] == "" {
		t.Fatalf("placeholder emails are skipped, the phone still gets WhatsApp: %v", c)
	}
}

func TestAutoTopupFailedTemplatesAreConsistent(t *testing.T) {
	raw, err := os.ReadFile("../../internal/whatsapp/templatesync/templates.json")
	if err != nil {
		t.Fatal(err)
	}
	var defs []struct {
		Name    string   `json:"name"`
		Body    string   `json:"body"`
		Example []string `json:"example"`
		Buttons []struct {
			URL string `json:"url"`
		} `json:"buttons"`
	}
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range defs {
		if d.Name != "credits_auto_topup_failed_v1_btn" {
			continue
		}
		found = true
		n := autoTopupFailedNotice(autoTopupEvent(), "t-1", &tenantInfo{Name: "Alpha"}, "a@b.co", []string{"0712345678"})
		if got := len(n.Parties[0].WhatsApp.Params); got != len(d.Example) || got != strings.Count(d.Body, "{{") {
			t.Fatalf("params %d, examples %d, placeholders %d", got, len(d.Example), strings.Count(d.Body, "{{"))
		}
		if len(d.Buttons) != 1 || d.Buttons[0].URL != pricingAppBase+"/{{1}}" {
			t.Fatalf("button must be a URL button on %s: %+v", pricingAppBase, d.Buttons)
		}
		if strings.HasPrefix(d.Body, "{{") || strings.HasSuffix(strings.TrimSpace(d.Body), "}}") {
			t.Fatal("Meta rejects a body that starts or ends with a parameter")
		}
	}
	if !found {
		t.Fatal("template missing from templates.json")
	}
	for _, p := range []string{"../../templates/email/subscription/credits_auto_topup_failed.html", "../../templates/whatsapp/subscription/credits_auto_topup_failed.txt"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing template file %s", p)
		}
	}
}
