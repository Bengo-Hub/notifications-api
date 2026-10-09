package events

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// The alert template renders the refusal facts the platform needs to act on (account, balance,
// fee) and the top up instruction, and drops the rows the event leaves empty.
func TestPayHeroWalletShortTemplateRenders(t *testing.T) {
	tpl, err := template.ParseFiles("../../../templates/email/" + payheroWalletShortTemplate + ".html")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	data := map[string]any{
		"tenant_id": "4414b8d9-6b00-4ad1-a0b4-3094cbc5e398", "vendor_id": int64(12663), "mode": "platform_root",
		"amount": "15000.00", "currency": "KES", "channel_fee": "55.00", "service_wallet_balance": "50.00",
		"personal": true, "intent_id": "92d0260c-0189-4e2d-916b-22c47d20a453", "reference": "",
		"window_h": 6, "refused_at": "2026-10-09 03:41 UTC", "dashboard_link": "https://app.payhero.africa",
	}
	if err := tpl.ExecuteTemplate(&out, "content", data); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{"account 12663", "KES 50.00", "KES 55.00", "KES 15000.00 (personal invoice)", "Top up service wallet", "every 6 hours"} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered alert lacks %q", want)
		}
	}
	if strings.Contains(html, "<strong>Reference</strong>") {
		t.Error("an empty reference must not render its row")
	}
}
