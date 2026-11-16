package handlers

import (
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/providers"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatecat"
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

// When Meta's webhook verdict arrives while the Test call waits, the answer says what happened to
// the message, not only that Meta accepted it (16 November 2026: tests "accepted" then failed
// with 131049 and nothing on screen said so).
func TestProviderTestMessageWithMetaVerdict(t *testing.T) {
	info := func(st templatecat.SendStatus) map[string]interface{} {
		return map[string]interface{}{"sent_as": providers.TestSentAsTemplate, "template": providers.WhatsAppTestTemplate, "delivery": st}
	}
	if got := providerTestMessage("whatsapp", "254700000000", info(templatecat.SendStatus{Status: "delivered"})); !strings.Contains(got, "delivered") {
		t.Errorf("delivered: %q", got)
	}
	got := providerTestMessage("whatsapp", "254700000000", info(templatecat.SendStatus{Status: "failed", Code: 131049, Title: "healthy ecosystem"}))
	if !strings.Contains(got, "131049") || !strings.Contains(got, "marketing") || strings.Contains(got, "accepted by Meta as the approved") {
		t.Errorf("131049: %q", got)
	}
	got = providerTestMessage("whatsapp", "254700000000", info(templatecat.SendStatus{Status: "failed", Code: 131026}))
	if !strings.Contains(got, "131026") || !strings.Contains(got, "cannot receive") {
		t.Errorf("131026: %q", got)
	}
	got = providerTestMessage("whatsapp", "254700000000", info(templatecat.SendStatus{Status: "failed", Code: 131000, Title: "Something went wrong"}))
	if !strings.Contains(got, "131000") || !strings.Contains(got, "Something went wrong") {
		t.Errorf("other: %q", got)
	}
}
