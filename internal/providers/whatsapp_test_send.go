package providers

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// WhatsAppTestTemplate is the approved utility template the Test button sends. It is in every
// tenant's shared template set (tenanttemplates selection.json), so the template sync puts it on
// each tenant's own WhatsApp account.
//
// v2 replaces notifications_test_message_v1 (16 November 2026). v1 was a generic notice tied to
// nothing the reader did, so Meta approved it as MARKETING: it was billed at the marketing rate and
// after the first test every later one to the same person failed with 131049, Meta's per person
// marketing limit. v2 is the reply to the test the person asked for, with its time and reference.
const WhatsAppTestTemplate = "notifications_test_message_v2"

// How the test went out, reported back as info["sent_as"].
const (
	TestSentAsTemplate = "template"
	TestSentAsText     = "text"
)

const whatsAppTestText = "Test message from Codevertex Notifications: your WhatsApp number is connected."

// nairobi is East Africa Time; a fixed zone so the test time never depends on the pod's tzdata.
var nairobi = time.FixedZone("EAT", 3*60*60)

// whatsAppTestParams are the template's {{1}} request time and {{2}} reference.
func whatsAppTestParams(now time.Time) []string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return []string{
		now.In(nairobi).Format("2 Jan 2006, 15:04 MST"),
		fmt.Sprintf("T-%X", b),
	}
}

// whatsAppTestSend sends the Test button's message. The free text it used to send was refused
// with #131047 unless the person had written to the business in the last 24 hours, so the test
// goes out as the approved utility template, which reaches any number (owner, 14 November 2026).
// Meta charges the tenant's account for it as a utility message; no Codevertex credits are used.
// Until the template is approved on the tenant's account (a 1320xx template error) the free text
// is sent instead, and any other error, such as a malformed number, is returned as it is.
//
// messageID is Meta's id for the template send (empty for the free text), so the caller can wait
// for Meta's later verdict on it: Meta accepting a message is not Meta delivering it.
func whatsAppTestSend(ctx context.Context, prov WhatsAppProvider, to string) (sentAs, messageID string, err error) {
	md := map[string]interface{}{
		"template_name":   WhatsAppTestTemplate,
		"template_params": whatsAppTestParams(time.Now()),
	}
	err = prov.SendWhatsApp(ctx, "", []string{to}, whatsAppTestText, md)
	if err == nil {
		id, _ := md[messaging.MetaSentMessageID].(string)
		return TestSentAsTemplate, id, nil
	}
	if !strings.Contains(err.Error(), "#1320") {
		return "", "", err
	}
	err = prov.SendWhatsApp(ctx, "", []string{to}, whatsAppTestText, map[string]interface{}{})
	if err != nil && strings.Contains(err.Error(), "131047") {
		return "", "", fmt.Errorf("your WhatsApp number is connected, but the test template %s is not approved on your WhatsApp account yet, "+
			"so the test went as a free reply, which Meta only allows within 24 hours of the person writing to the business: "+
			"sync the templates under Settings > Providers > WhatsApp and wait for approval, or send any WhatsApp message to your business number from %s, then press Test again",
			WhatsAppTestTemplate, to)
	}
	if err != nil {
		return "", "", err
	}
	return TestSentAsText, "", nil
}
