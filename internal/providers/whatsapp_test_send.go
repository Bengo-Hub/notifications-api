package providers

import (
	"context"
	"fmt"
	"strings"
)

// WhatsAppTestTemplate is the approved utility template the Test button sends. It has no params and
// is in every tenant's shared template set (tenanttemplates selection.json), so the template sync
// puts it on each tenant's own WhatsApp account.
const WhatsAppTestTemplate = "notifications_test_message_v1"

// How the test went out, reported back as info["sent_as"].
const (
	TestSentAsTemplate = "template"
	TestSentAsText     = "text"
)

const whatsAppTestText = "Test message from Codevertex Notifications: your WhatsApp number is connected."

// whatsAppTestSend sends the Test button's message. The free text it used to send was refused
// with #131047 unless the person had written to the business in the last 24 hours, so the test
// now goes out as the approved utility template, which reaches any number (owner, 14 November
// 2026; this replaces the 12 November rule that tests are only ever free text). Meta charges the
// tenant's account for it as a utility message; no Codevertex credits are used. Until the template
// is approved on the tenant's account (a 1320xx template error) the free text is sent instead, and
// any other error, such as a malformed number, is returned as it is.
func whatsAppTestSend(ctx context.Context, prov WhatsAppProvider, to string) (string, error) {
	err := prov.SendWhatsApp(ctx, "", []string{to}, whatsAppTestText, map[string]interface{}{
		"template_name": WhatsAppTestTemplate,
	})
	if err == nil {
		return TestSentAsTemplate, nil
	}
	if !strings.Contains(err.Error(), "#1320") {
		return "", err
	}
	err = prov.SendWhatsApp(ctx, "", []string{to}, whatsAppTestText, map[string]interface{}{})
	if err != nil && strings.Contains(err.Error(), "131047") {
		return "", fmt.Errorf("your WhatsApp number is connected, but the test template %s is not approved on your WhatsApp account yet, "+
			"so the test went as a free reply, which Meta only allows within 24 hours of the person writing to the business: "+
			"sync the templates under Settings > Providers > WhatsApp and wait for approval, or send any WhatsApp message to your business number from %s, then press Test again",
			WhatsAppTestTemplate, to)
	}
	if err != nil {
		return "", err
	}
	return TestSentAsText, nil
}
