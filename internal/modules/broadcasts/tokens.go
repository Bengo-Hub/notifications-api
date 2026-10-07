// Package broadcasts sends one notice to many people (the platform to its tenants, a tenant to its
// customers or staff) over email, SMS, WhatsApp, push and in-app banners, with approval, consent
// and opt-out checks, and paced under each provider's limits.
package broadcasts

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tokens a message may use. Values come from the recipient (first_name, name, business_name) and
// the sender (sender_name), plus the occasion being greeted.
var allowedTokens = map[string]bool{
	"first_name":    true,
	"name":          true,
	"business_name": true,
	"sender_name":   true,
	"year":          true,
	"occasion":      true,
}

var tokenRe = regexp.MustCompile(`\{([a-z_]+)\}`)

// UnknownTokens lists the {tokens} in text that are not allowed, so a typo ("{firstname}") is
// caught when the message is saved rather than sent to thousands of people as written.
func UnknownTokens(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range tokenRe.FindAllStringSubmatch(text, -1) {
		if !allowedTokens[m[1]] && !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// Vars are the personalisation values for one recipient.
type Vars struct {
	FirstName    string
	Name         string
	BusinessName string
	SenderName   string
	Occasion     string
	Year         int
}

// Map is the form stored on a recipient row.
func (v Vars) Map() map[string]any {
	return map[string]any{
		"first_name":    v.FirstName,
		"name":          v.Name,
		"business_name": v.BusinessName,
		"sender_name":   v.SenderName,
		"occasion":      v.Occasion,
		"year":          v.Year,
	}
}

// VarsFromMap reads a recipient row's vars.
func VarsFromMap(m map[string]any) Vars {
	s := func(k string) string { v, _ := m[k].(string); return v }
	y := 0
	switch n := m["year"].(type) {
	case float64:
		y = int(n)
	case int:
		y = n
	}
	return Vars{FirstName: s("first_name"), Name: s("name"), BusinessName: s("business_name"),
		SenderName: s("sender_name"), Occasion: s("occasion"), Year: y}
}

// greetingName is who "Dear ..." addresses: the person's first name, else their business
// ("Urban Loft team"), else a neutral "there".
func (v Vars) greetingName() string {
	if n := strings.TrimSpace(v.FirstName); n != "" {
		return n
	}
	if b := strings.TrimSpace(v.BusinessName); b != "" {
		return b + " team"
	}
	if n := strings.TrimSpace(v.Name); n != "" {
		return n
	}
	return "there"
}

// Render fills the tokens in text. Plain substitution, not a template language: message text is
// written by tenants and must never be able to run anything.
func Render(text string, v Vars) string {
	year := v.Year
	if year == 0 {
		year = time.Now().Year()
	}
	name := strings.TrimSpace(v.Name)
	if name == "" {
		name = v.greetingName()
	}
	repl := map[string]string{
		"first_name":    v.greetingName(),
		"name":          name,
		"business_name": strings.TrimSpace(v.BusinessName),
		"sender_name":   strings.TrimSpace(v.SenderName),
		"year":          strconv.Itoa(year),
		"occasion":      strings.TrimSpace(v.Occasion),
	}
	return tokenRe.ReplaceAllStringFunc(text, func(m string) string {
		key := m[1 : len(m)-1]
		if val, ok := repl[key]; ok {
			return val
		}
		return m
	})
}

// smsOptOut is appended to marketing SMS (Kenya CA consumer protection rules: a free opt-out in
// the message itself).
const smsOptOut = "Reply STOP to opt out"

// WithSMSOptOut adds the opt-out line to a marketing SMS unless the text already has one.
func WithSMSOptOut(body string) string {
	if strings.Contains(strings.ToUpper(body), "STOP") {
		return body
	}
	return strings.TrimRight(body, " \n") + "\n" + smsOptOut
}

// ValidateText rejects unknown tokens in every text field of a broadcast's content.
func ValidateText(fields map[string]string) error {
	var bad []string
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, t := range UnknownTokens(fields[k]) {
			bad = append(bad, fmt.Sprintf("{%s} in %s", t, k))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("unknown placeholders: %s (use {first_name}, {name}, {business_name}, {sender_name}, {year} or {occasion})", strings.Join(bad, ", "))
	}
	return nil
}
