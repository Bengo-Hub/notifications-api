package email

import (
	"html"
	"regexp"
	"strings"
)

var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</\s*(script|style)\s*>`)
	reBreakTags   = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/h[1-6])\s*/?>`)
	reAnyTag      = regexp.MustCompile(`(?s)<[^>]+>`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

// HTMLToPlainText derives a reasonable text/plain alternative from an HTML
// email body — every production email sent by this service was HTML-only
// (confirmed 2026-08-19 deliverability audit: cmd/worker/main.go always
// passed textBody="") with no multipart/alternative part at all, itself a
// real spam signal. Not a full HTML renderer (tables/lists collapse to plain
// lines) — good enough for a text alternative, which by RFC 8621/real mail
// client convention is only ever a fallback for the HTML part anyway.
func HTMLToPlainText(htmlBody string) string {
	if htmlBody == "" {
		return ""
	}
	s := reScriptStyle.ReplaceAllString(htmlBody, "")
	s = reBreakTags.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
