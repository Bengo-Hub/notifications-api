package whatsappinbox

import (
	"testing"
	"unicode/utf8"
)

func TestInboundPlaceholder(t *testing.T) {
	cases := map[string]string{
		"image":       "Photo",
		"IMAGE ":      "Photo",
		"audio":       "Voice note",
		"sticker":     "Sticker",
		"unknown":     "Message (open WhatsApp to view)",
		"unsupported": "A message WhatsApp does not share with the inbox (for example view-once media, a poll or an edit). Read it on the phone, or ask them to send it again as text.",
		"":            "Message (open WhatsApp to view)",
	}
	for in, want := range cases {
		if got := inboundPlaceholder(in); got != want {
			t.Errorf("inboundPlaceholder(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateRunesKeepsCharactersWhole(t *testing.T) {
	s := "Vé😘e💘e" // multi-byte characters near the cut
	for n := 0; n <= 8; n++ {
		got := truncateRunes(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateRunes(%q, %d) = %q is not valid UTF-8", s, n, got)
		}
		if utf8.RuneCountInString(got) > n {
			t.Fatalf("truncateRunes(%q, %d) kept %d characters", s, n, utf8.RuneCountInString(got))
		}
	}
	if truncateRunes("short", 140) != "short" {
		t.Error("a short string must come back unchanged")
	}
}
