package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Bengo-Hub/shared-events/balancealert"
	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// The event as subscriptions-api and marketflow-ai publish it (shared-events envelope).
func balanceEvent(t *testing.T, a balancealert.Alert) balanceLowEvent {
	t.Helper()
	evt, err := a.Event()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := evt.ToJSON()
	var out balanceLowEvent
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBalanceLowTenantNotice(t *testing.T) {
	tid := uuid.New()
	evt := balanceEvent(t, balancealert.Alert{Scope: "tenant", TenantID: tid.String(), Resource: "ai_credits", ResourceLabel: "AI credits",
		Level: "low", Remaining: 19, Unit: "credits", Threshold: 20, Period: "2026-11", BuyURL: "https://pricing.codevertexafrica.com/billing"})
	if evt.Payload.Resource != "ai_credits" || evt.Payload.Remaining != 19 {
		t.Fatalf("decoded = %+v", evt.Payload)
	}
	n := balanceLowNotice(evt, &tenantInfo{ID: tid, Name: "Alpha"}, "owner@alpha.co", []string{"0712345678"})
	if n.TemplateID != "billing/balance_low" || n.Scope != messaging.SenderScopePlatform || n.TenantID != tid.String() {
		t.Fatalf("notice = %+v", n)
	}
	p := n.Parties[0]
	if p.Target != messaging.TargetTenantAdmin || p.WhatsApp.Name != "credits_balance_low_v1_btn" || p.WhatsApp.Button != "billing" {
		t.Fatalf("party = %+v", p)
	}
	if got := strings.Join(p.WhatsApp.Params, "|"); got != "Alpha|AI credits|is low|19 credits" {
		t.Fatalf("params = %s", got)
	}
	if n.Data["action_link"] != pricingAppBase+"/billing" || !strings.Contains(n.Subject, "running low") {
		t.Fatalf("data = %v subject = %s", n.Data, n.Subject)
	}
	if c := partyContacts(p); c["email"] == "" || c["whatsapp"] == "" {
		t.Fatalf("both channels expected: %v", c)
	}
}

func TestBalanceLowPlatformNotice(t *testing.T) {
	evt := balanceEvent(t, balancealert.Alert{Scope: "platform", Resource: "higgsfield", ResourceLabel: "Higgsfield",
		Level: "empty", Remaining: 0, Unit: "usd", Threshold: 5, Period: "2026-11-03", Estimate: true, BuyURL: "https://console.higgsfield.ai"})
	n := balanceLowNotice(evt, &tenantInfo{ID: uuid.New(), Name: "Codevertex"}, "owner@codevertex.example", nil)
	p := n.Parties[0]
	if n.TemplateID != "billing/provider_balance_low" || p.Target != messaging.TargetPlatformAdmin || p.WhatsApp.Name != "provider_balance_low_v1_btn" {
		t.Fatalf("notice = %+v", n)
	}
	if got := strings.Join(p.WhatsApp.Params, "|"); got != "Higgsfield|has run out|about USD 0.00" {
		t.Fatalf("params = %s", got)
	}
	if n.Data["action_link"] != "https://console.higgsfield.ai" || n.Data["providers_link"] != marketflowAppBase+"/platform/providers" {
		t.Fatalf("links = %v", n.Data)
	}
	if c := partyContacts(p); c["email"] == "" || c["whatsapp"] != "" {
		t.Fatalf("email only without a phone: %v", c)
	}

	// PLATFORM_ALERT_EMAILS take the email; the owner's phone keeps the WhatsApp message.
	n = balanceLowNotice(evt, &tenantInfo{ID: uuid.New(), Name: "Codevertex"}, "owner@codevertex.example", []string{"0712345678"},
		"ops@codevertex.example", " ops@codevertex.example", "bad")
	if len(n.Parties) != 2 || n.Parties[1].Email != "ops@codevertex.example" {
		t.Fatalf("parties = %+v", n.Parties)
	}
	if c := partyContacts(n.Parties[0]); c["email"] != "" || c["whatsapp"] == "" {
		t.Fatalf("owner party = %v", c)
	}
}

func TestBalanceLowTemplatesAreConsistent(t *testing.T) {
	raw, err := os.ReadFile("../../internal/whatsapp/templatesync/templates.json")
	if err != nil {
		t.Fatal(err)
	}
	var defs []struct {
		Name     string   `json:"name"`
		Category string   `json:"category"`
		Body     string   `json:"body"`
		Example  []string `json:"example"`
		Buttons  []struct {
			URL string `json:"url"`
		} `json:"buttons"`
	}
	if err := json.Unmarshal(raw, &defs); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		params int
		base   string
	}{
		"credits_balance_low_v1_btn":  {4, pricingAppBase},
		"provider_balance_low_v1_btn": {3, marketflowAppBase},
	}
	for _, d := range defs {
		w, ok := want[d.Name]
		if !ok {
			continue
		}
		delete(want, d.Name)
		if d.Category != "UTILITY" || len(d.Example) != w.params || strings.Count(d.Body, "{{") != w.params {
			t.Fatalf("%s: category %s, %d examples, %d placeholders", d.Name, d.Category, len(d.Example), strings.Count(d.Body, "{{"))
		}
		if len(d.Buttons) != 1 || d.Buttons[0].URL != w.base+"/{{1}}" {
			t.Fatalf("%s: button must be a URL button on %s: %+v", d.Name, w.base, d.Buttons)
		}
		if strings.HasPrefix(d.Body, "{{") || strings.HasSuffix(strings.TrimSpace(d.Body), "}}") {
			t.Fatalf("%s: Meta rejects a body that starts or ends with a parameter", d.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing from templates.json: %v", want)
	}
	for _, p := range []string{"../../templates/email/billing/balance_low.html", "../../templates/email/billing/provider_balance_low.html",
		"../../templates/whatsapp/billing/balance_low.txt", "../../templates/whatsapp/billing/provider_balance_low.txt"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing template file %s", p)
		}
	}
	if s := formatCount(12345); s != "12,345" {
		t.Fatalf("count = %s", s)
	}
}
