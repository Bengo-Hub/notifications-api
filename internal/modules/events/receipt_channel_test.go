package events

import "testing"

// Sale receipts go by email or WhatsApp only; a legacy "sms" or empty request never picks SMS.
func TestReceiptChannelNeverSMS(t *testing.T) {
	cases := []struct{ requested, phone, email, want string }{
		{"", "0712", "a@b.co", "email"},
		{"", "0712", "", "whatsapp"},
		{"sms", "0712", "", "whatsapp"},
		{"SMS", "0712", "a@b.co", "email"},
		{"whatsapp", "0712", "a@b.co", "whatsapp"},
		{"whatsapp", "", "a@b.co", "email"},
		{"email", "0712", "", "whatsapp"},
	}
	for _, c := range cases {
		if got := receiptChannel(c.requested, c.phone, c.email); got != c.want {
			t.Errorf("receiptChannel(%q, %q, %q) = %q, want %q", c.requested, c.phone, c.email, got, c.want)
		}
	}
}
