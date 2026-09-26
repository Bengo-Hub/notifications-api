package events

import "testing"

// A shared receipt must go out with the approved button template (the short code as the button
// suffix), since a customer at the till has rarely messaged the business on WhatsApp.
func TestReceiptShareWhatsApp(t *testing.T) {
	m := receiptShareWhatsApp("David", "POS-88213", 1180, "KES", "https://r.codevertexafrica.com/7Kq9mZ2xT4bNw1")
	if m["template_name"] != "pos_receipt_share_btn" || m["template_button_param"] != "7Kq9mZ2xT4bNw1" {
		t.Fatalf("short link should use the button template: %#v", m)
	}
	params, _ := m["template_params"].([]string)
	if len(params) != 3 || params[2] != "KES 1,180" {
		t.Fatalf("unexpected params: %#v", params)
	}
	if got := receiptShareWhatsApp("", "", 50.5, "", "https://r.codevertexafrica.com/abc")["template_params"].([]string); got[0] != "there" || got[1] != "your order" || got[2] != "KES 50.50" {
		t.Fatalf("empty values need readable defaults: %#v", got)
	}
	other := receiptShareWhatsApp("David", "POS-1", 10, "KES", "https://posapi.example.com/api/v1/r/abc")
	if other["template_name"] != nil || other["cta_button_url"] != "https://posapi.example.com/api/v1/r/abc" {
		t.Fatalf("a link on another host keeps the interactive button: %#v", other)
	}
}
