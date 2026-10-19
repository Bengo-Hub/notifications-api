package credits

// SMSSegments calculates the number of billable segments for an SMS body.
// GSM-7: 160 chars for single segment, 153 for multi-segment (UDH).
// Unicode (UCS-2): 70 chars for single segment, 67 for multi-segment (UDH).
func SMSSegments(body string) int {
	if body == "" {
		return 0
	}

	isGSM := isGSM7(body)
	length := len([]rune(body))

	if isGSM {
		if length <= 160 {
			return 1
		}
		// Multi-segment GSM: 153 chars per segment
		return (length + 152) / 153
	}

	// Unicode
	if length <= 70 {
		return 1
	}
	// Multi-segment Unicode: 67 chars per segment
	return (length + 66) / 67
}

// isGSM7 checks if a string can be encoded entirely in GSM-7.
func isGSM7(s string) bool {
	for _, r := range s {
		if !isGSM7Rune(r) {
			return false
		}
	}
	return true
}

func isGSM7Rune(r rune) bool {
	// GSM-7 Basic Character Set + Extension Set
	switch r {
	case '@', '£', '$', '¥', 'è', 'é', 'ù', 'ì', 'ò', 'Ç', '\n', 'Ø', 'ø', '\r', 'Å', 'å',
		'Δ', '_', 'Φ', 'Γ', 'Λ', 'Ω', 'Π', 'Ψ', 'Σ', 'Θ', 'Ξ', ' ', '!', '"', '#', '¤', '%', '&', '\'', '(', ')', '*', '+', ',', '-', '.', '/',
		'0', '1', '2', '3', '4', '5', '6', '7', '8', '9', ':', ';', '<', '=', '>', '?',
		'¡', 'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O', 'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z', 'Ä', 'Ö', 'Ñ', 'Ü', '§',
		'¿', 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z', 'ä', 'ö', 'ñ', 'ü', 'à',
		// Extension Set (escaped with 0x1B)
		'|', '^', '€', '{', '}', '[', '~', ']', '\\':
		return true
	}
	return false
}
