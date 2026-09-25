package main

import (
	"regexp"
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
		if m.WhatsAppTemplate == "" || m.WhatsAppParams == nil {
			continue
		}
		params := m.WhatsAppParams(sample)
		check(event, m.WhatsAppTemplate, len(params))
		if m.WhatsAppButtonTemplate != "" {
			check(event, m.WhatsAppButtonTemplate, len(params)-1) // the link becomes the button
		}
		if m.WhatsAppOriginalTemplate != "" && m.WhatsAppOriginalParams != nil {
			check(event, m.WhatsAppOriginalTemplate, len(m.WhatsAppOriginalParams(sample)))
		}
	}
	// The business alert sends five parameters.
	check("business alert", "ordering_new_order_business_v1", 5)
}
