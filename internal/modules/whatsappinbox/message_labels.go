package whatsappinbox

import "strings"

// mediaLabels are what the inbox shows for messages with no text of their own. Staff read
// "Photo", not "[unsupported message type: image]".
var mediaLabels = map[string]string{
	"image":       "Photo",
	"video":       "Video",
	"audio":       "Voice note",
	"voice":       "Voice note",
	"document":    "Document",
	"sticker":     "Sticker",
	"location":    "Location",
	"contacts":    "Contact card",
	"reaction":    "Reaction",
	"interactive": "Button reply",
	"button":      "Button reply",
	"order":       "Order",
}

// inboundPlaceholder is the stored body for an inbound message that carried no text.
func inboundPlaceholder(msgType string) string {
	if label, ok := mediaLabels[strings.ToLower(strings.TrimSpace(msgType))]; ok {
		return label
	}
	return "Message (open WhatsApp to view)"
}

// truncateRunes shortens s to at most n characters without splitting a multi-byte character
// (names and messages often contain emoji).
func truncateRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
