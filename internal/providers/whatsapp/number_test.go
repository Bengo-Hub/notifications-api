package whatsapp

import "testing"

func TestInternationalWhatsAppNumber(t *testing.T) {
	cases := []struct {
		raw, dial, want string
	}{
		{"0712 345 678", "", "254712345678"},
		{"0712-345-678", "256", "256712345678"},
		{"712345678", "", "254712345678"},
		{"0110345678", "", "254110345678"},
		{"+254 712 345 678", "", "254712345678"},
		{"254712345678", "256", "254712345678"},
		{"00256772123456", "", "256772123456"},
		{"+1 (415) 555-0100", "", "14155550100"},
	}
	for _, tc := range cases {
		if got := internationalWhatsAppNumber(tc.raw, tc.dial); got != tc.want {
			t.Fatalf("internationalWhatsAppNumber(%q, %q) = %q, want %q", tc.raw, tc.dial, got, tc.want)
		}
	}
}
