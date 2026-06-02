package messaging

import (
	"net/mail"
	"strings"
)

// NormalizeRecipients flattens recipient inputs into clean individual addresses.
//
// Callers sometimes pass a single element containing several addresses joined by
// commas, semicolons or newlines (e.g. from a textarea or a comma-separated UI
// field). Sending such a joined element verbatim as one SMTP RCPT TO produces a
// "501 invalid address" and the whole message is dropped. This splits every input
// element on comma/semicolon/newline, trims, drops blanks, skips fragments that are
// not parseable email addresses, and dedupes case-insensitively while preserving order.
func NormalizeRecipients(in []string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(in))
	for _, raw := range in {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\r'
		}) {
			addr := strings.TrimSpace(part)
			if addr == "" {
				continue
			}
			if _, err := mail.ParseAddress(addr); err != nil {
				continue // skip a malformed fragment rather than 501 the whole batch
			}
			key := strings.ToLower(addr)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, addr)
		}
	}
	return out
}
