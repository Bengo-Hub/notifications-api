package handlers

import (
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/providers"
)

// The Test button's answer says how the WhatsApp test went out: the approved template reaches any
// number, the free text only inside the 24 hour window.
func TestProviderTestMessage(t *testing.T) {
	tpl := providerTestMessage("whatsapp", "254700000000", map[string]interface{}{"sent_as": providers.TestSentAsTemplate, "template": providers.WhatsAppTestTemplate})
	if !strings.Contains(tpl, providers.WhatsAppTestTemplate) || strings.Contains(tpl, "24 hours") {
		t.Errorf("template: %q", tpl)
	}
	text := providerTestMessage("whatsapp", "254700000000", map[string]interface{}{"sent_as": providers.TestSentAsText, "template": providers.WhatsAppTestTemplate})
	if !strings.Contains(text, "24 hours") || !strings.Contains(text, "not approved") {
		t.Errorf("text: %q", text)
	}
	if got := providerTestMessage("whatsapp", "", map[string]interface{}{"status": "CONNECTED"}); got != "connection verified" {
		t.Errorf("account check: %q", got)
	}
	if got := providerTestMessage("sms", "254700000000", nil); got != "test message sent successfully" {
		t.Errorf("sms: %q", got)
	}
}
