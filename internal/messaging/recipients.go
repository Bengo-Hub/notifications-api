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
// element on comma/semicolon/newline, trims, drops blanks, and dedupes
// case-insensitively while preserving order.
//
// channel controls address validation: only "email" recipients are actual RFC 5322
// email addresses, so mail.ParseAddress only runs for that channel. Every other
// channel (sms, whatsapp, push, webhook, ...) gets a phone number, device token, or
// URL — running email validation on those unconditionally silently discarded every
// non-email-shaped recipient (e.g. every phone number), which is exactly the bug this
// channel parameter fixes: SMS/WhatsApp sends were being queued with an empty
// recipient list and delivering to nobody.
//
// Push recipients are device tokens and are never split: a Web Push token is the whole
// subscription JSON ({"endpoint":...,"keys":{"p256dh":...,"auth":...}}), and splitting it on
// commas turned one subscription into several bogus "tokens" that were sent to FCM and logged as
// recipients with the subscription keys in plain text.
func NormalizeRecipients(in []string, channel string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(in))
	split := func(raw string) []string {
		if channel == "push" {
			return []string{raw}
		}
		return strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == '\n' || r == '\r'
		})
	}
	for _, raw := range in {
		for _, part := range split(raw) {
			addr := strings.TrimSpace(part)
			if addr == "" {
				continue
			}
			if channel == "email" {
				if _, err := mail.ParseAddress(addr); err != nil {
					continue // skip a malformed fragment rather than 501 the whole batch
				}
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
