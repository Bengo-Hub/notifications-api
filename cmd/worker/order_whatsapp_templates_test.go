package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// TestOrderWhatsAppParamsMatchManifest guards against the silent failure mode of WhatsApp templates:
// Meta rejects a send whose parameter count differs from the approved template, and the customer
// simply never gets the message. Every ordering mapping must send exactly as many body parameters
// as its template (and its button / fallback variants) declares in templates.json.
func TestOrderWhatsAppParamsMatchManifest(t *testing.T) {
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
	sample := map[string]interface{}{
		"name": "Amina", "order_number": "ORD-1", "order_id": "x", "total_amount": "KES 100",
		"order_link": "https://ordering.codevertexafrica.com/t/orders/guest/x", "track_link": "https://ordering.codevertexafrica.com/t/orders/guest/x",
		"review_link": "https://ordering.codevertexafrica.com/t/orders/guest/x?rate=1", "rider_name": "James", "rider_phone": "0711",
		"cancel_reason": "out of stock", "amount": "KES 100", "reason": "r", "scheduled_for": "18:00",
		"outlet_name": "Westlands", "pickup_time": "now",
	}
	check := func(event, template string, got int) {
		want, ok := bodyParams[template]
		if !ok {
			t.Errorf("%s: template %q is not in templates.json", event, template)
			return
		}
		if got != want {
			t.Errorf("%s: template %q expects %d parameters, mapping sends %d", event, template, want, got)
		}
	}
	for event, m := range orderMappings {
		if m.WhatsAppParams == nil {
			continue
		}
		params := m.WhatsAppParams(sample)
		if m.WhatsAppTemplate != "" {
			check(event, m.WhatsAppTemplate, len(params))
		}
		if m.WhatsAppButtonTemplate != "" {
			check(event, m.WhatsAppButtonTemplate, len(params)-1) // the link becomes the button
		}
	}
	// The business alert: four body parameters, the queue link is the Open Orders button.
	check("business alert", "ordering_new_order_business_v1_btn", 4)
}

// LINK POLICY: a message with a link is always sent with its button template (every tenant,
// including one on its own domain), and a template sent without a button never shows a URL.
func TestOrderWhatsAppLinkPolicy(t *testing.T) {
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]templatesync.TemplateDef{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	looksLikeURL := func(s string) bool {
		return strings.Contains(s, "://") || strings.Contains(s, ".com/") || strings.Contains(s, ".co/")
	}

	for _, link := range []string{
		"https://ordering.codevertexafrica.com/urban-loft/orders/guest/abc",
		"https://theurbanloftcafe.com/urban-loft/orders/guest/abc?rate=1", // tenant on its own domain
	} {
		for event, m := range orderMappings {
			if m.WhatsAppParams == nil {
				continue
			}
			data := map[string]interface{}{"order_link": link, "track_link": link, "review_link": link, "name": "Amina", "order_number": "000006"}
			meta := whatsAppTemplateMetadata(m, data)
			name, _ := meta["template_name"].(string)
			def, ok := byName[name]
			if name != "" && !ok {
				t.Errorf("%s: sends %q, which is not in templates.json", event, name)
				continue
			}
			if m.WhatsAppLinkKey != "" {
				if len(def.Buttons) == 0 {
					t.Errorf("%s (%s): message has a link but is sent with %q, which has no URL button", event, link, name)
				}
				if meta["template_button_param"] == "" {
					t.Errorf("%s: button suffix missing", event)
				}
				continue
			}
			for _, ex := range def.Example {
				if looksLikeURL(ex) {
					t.Errorf("%s: %q is sent without a button but its body carries a link", event, name)
				}
			}
		}
	}

	suffix := buttonURLSuffix("https://theurbanloftcafe.com/urban-loft/orders/guest/abc?rate=1")
	if suffix != "urban-loft/orders/guest/abc?rate=1" {
		t.Fatalf("custom-domain link must map onto the shared domain path, got %q", suffix)
	}
}
