package main

import (
	"fmt"
	"strings"
)

// WhatsApp link policy (docs/whatsapp-template-policy.md, .claude/memory): a WhatsApp message never
// shows a raw URL in its text. Template sends use a Meta template with a URL button ("_btn");
// free-form sends (inside the customer's 24h reply window) use an interactive message with one
// tappable button. The WhatsApp text bodies under templates/whatsapp/ therefore never include the
// link; this attaches it as the button.

// whatsAppLinkFields are the message data keys that carry a link, in priority order, with the
// button label shown for each.
var whatsAppLinkFields = []struct{ key, label string }{
	{"cta_url", "Open"},
	{"order_link", "View Order"},
	{"track_link", "Track Order"},
	{"tracking_link", "Track Delivery"},
	{"review_link", "Leave Feedback"},
	{"feedback_link", "Leave Feedback"},
	{"pay_link", "Pay Now"},
	{"retry_link", "Try Again"},
	{"download_link", "View Receipt"},
	{"receipt_link", "View Receipt"},
	{"manage_link", "Open Orders"},
	{"job_link", "Open Job"},
	{"portal_url", "Open Portal"},
	{"action_link", "View"},
}

// applyWhatsAppLinkButton sets cta_button_text/cta_button_url for a free-form send whose data has a
// link. Template sends (template_name set) and sends that already chose a button are left alone.
func applyWhatsAppLinkButton(metadata map[string]interface{}, data map[string]interface{}) {
	if name, _ := metadata["template_name"].(string); name != "" {
		return
	}
	if u, _ := metadata["cta_button_url"].(string); u != "" {
		return
	}
	for _, f := range whatsAppLinkFields {
		v, ok := data[f.key]
		if !ok || v == nil {
			continue
		}
		link := strings.TrimSpace(fmt.Sprint(v))
		if !strings.HasPrefix(link, "https://") && !strings.HasPrefix(link, "http://") {
			continue
		}
		metadata["cta_button_url"] = link
		if text, _ := metadata["cta_button_text"].(string); text == "" {
			metadata["cta_button_text"] = f.label
		}
		return
	}
}
