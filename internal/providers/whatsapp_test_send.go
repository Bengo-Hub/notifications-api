package providers

import (
	"context"
	"fmt"
	"strings"
)

// whatsAppTestSend sends the Test button's message as a plain text (a service message), which Meta
// does not charge for, and never a template: Meta's sample "hello_world" only works from its public
// test numbers (#131058) and any real template is a paid message (owner, 2026-11-12: tests are never
// charged). Meta delivers plain text only within 24 hours of the person writing to the business
// (#131047); outside that window the error says how to test.
func whatsAppTestSend(ctx context.Context, prov WhatsAppProvider, to string) error {
	err := prov.SendWhatsApp(ctx, "", []string{to},
		"Test message from Codevertex Notifications: your WhatsApp number is connected.", map[string]interface{}{})
	if err != nil && strings.Contains(err.Error(), "131047") {
		return fmt.Errorf("your WhatsApp number is connected. Test messages are sent free as a reply, which Meta only allows within 24 hours of the person writing to the business: send any WhatsApp message to your business number from %s, then press Test again", to)
	}
	return err
}
