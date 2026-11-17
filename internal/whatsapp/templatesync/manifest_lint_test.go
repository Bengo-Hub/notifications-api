package templatesync

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

var (
	placeholder  = regexp.MustCompile(`\{\{(\d+)\}\}`)
	templateName = regexp.MustCompile(`^[a-z0-9_]{1,512}$`)
	// rawLink matches a domain-with-path example value such as "cdvtx.co/t/2291".
	rawLink = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+/`)
)

// TestManifestFollowsMetaRules checks every template against the formatting rules Meta applies
// at review, so a bad template fails here instead of being rejected days later: body variables
// numbered 1..n with one example each, a body that neither starts nor ends with a variable, and
// URL buttons on https with at most one trailing {{1}} suffix and an example for it.
func TestManifestFollowsMetaRules(t *testing.T) {
	defs, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range defs {
		if !templateName.MatchString(d.Name) {
			t.Errorf("%s: name must be lowercase letters, digits and underscores", d.Name)
		}
		if seen[d.Name+"/"+d.Language] {
			t.Errorf("%s: duplicate name and language", d.Name)
		}
		seen[d.Name+"/"+d.Language] = true
		if d.Category == "AUTHENTICATION" || d.Body == "" {
			continue
		}

		matches := placeholder.FindAllStringSubmatch(d.Body, -1)
		maxN := 0
		nums := map[string]bool{}
		for _, m := range matches {
			nums[m[1]] = true
		}
		for i := 1; ; i++ {
			if !nums[strconv.Itoa(i)] {
				break
			}
			maxN = i
		}
		if maxN != len(nums) {
			t.Errorf("%s: body variables must be numbered 1..n without gaps", d.Name)
		}
		if len(d.Example) != len(nums) {
			t.Errorf("%s: %d body variables but %d examples", d.Name, len(nums), len(d.Example))
		}
		// Link policy (docs/whatsapp-template-policy.md): links are URL buttons, never body text.
		for i, ex := range d.Example {
			if strings.Contains(ex, "://") || rawLink.MatchString(ex) {
				t.Errorf("%s: body variable {{%d}} is a link (%q); put it in a URL button instead", d.Name, i+1, ex)
			}
		}
		// Meta error 2388299: a variable followed (or preceded) only by punctuation still counts as
		// leading or trailing ("Thanks {{1}}!" is rejected), so punctuation is ignored here.
		// 2026-10-07: seven occasion templates ending "...at {{2}}." failed this way.
		body := strings.Trim(strings.TrimSpace(d.Body), " \n.,!?:;")
		if strings.HasPrefix(body, "{{") {
			t.Errorf("%s: body must not start with a variable (punctuation does not count as text)", d.Name)
		}
		if strings.HasSuffix(body, "}}") {
			t.Errorf("%s: body must not end with a variable (punctuation does not count as text)", d.Name)
		}

		// Marketing templates (greetings, offers) must let the person opt out from the message
		// itself: Meta's "Stop promotions" quick reply, which the webhook turns into a suppression.
		if d.Category == "MARKETING" {
			optOut := false
			for _, b := range d.Buttons {
				if b.Type == "QUICK_REPLY" && b.Text == "Stop promotions" {
					optOut = true
				}
			}
			if !optOut {
				t.Errorf("%s: marketing templates need a QUICK_REPLY \"Stop promotions\" button", d.Name)
			}
		}

		for _, b := range d.Buttons {
			if b.Type == "QUICK_REPLY" && len([]rune(b.Text)) > 25 {
				t.Errorf("%s: quick reply text %q is over 25 characters", d.Name, b.Text)
			}
			if b.Type != "URL" {
				continue
			}
			if len([]rune(b.Text)) > 25 {
				t.Errorf("%s: button text %q is over 25 characters", d.Name, b.Text)
			}
			if !strings.HasPrefix(b.URL, "https://") {
				t.Errorf("%s: button URL must be https", d.Name)
			}
			vars := placeholder.FindAllString(b.URL, -1)
			switch {
			case len(vars) > 1 || (len(vars) == 1 && (vars[0] != "{{1}}" || !strings.HasSuffix(b.URL, "{{1}}"))):
				t.Errorf("%s: button URL may only end with a single {{1}}", d.Name)
			case len(vars) == 1 && len(b.Example) != 1:
				t.Errorf("%s: dynamic button URL needs exactly one example", d.Name)
			}
			if base := strings.TrimSuffix(b.URL, "{{1}}"); !buttonDomains[base] {
				t.Errorf("%s: button URL %q is not on a domain in docs/whatsapp-template-policy.md section 2", d.Name, b.URL)
			}
		}
	}
}

// buttonDomains are the button URL bases in docs/whatsapp-template-policy.md section 2. Meta fixes a
// button's domain at approval, so a new domain is a policy decision: add its row there first.
var buttonDomains = map[string]bool{
	"https://ordering.codevertexafrica.com/":   true,
	"https://pos.codevertexafrica.com/":        true,
	"https://r.codevertexafrica.com/":          true,
	"https://books.codevertexafrica.com/":      true,
	"https://pricing.codevertexafrica.com/":    true,
	"https://marketflow.codevertexafrica.com/": true,
	"https://maskaniapp.codevertexafrica.com/": true,
	"https://hadiafrica.com/":                  true,
	"https://riderapp.codevertexafrica.com/":   true,
	"https://logistics.codevertexafrica.com/":  true,
	"https://codevertexafrica.com/":            true,
	"https://projects.codevertexafrica.com/":   true,
	"https://ticketing.codevertexafrica.com/":  true,
}

// bannedUtilityWords are the words docs/whatsapp-template-policy.md section 3 keeps out of UTILITY
// templates: Meta reads them as a login credential or a promotion and moves the template to
// MARKETING or refuses it as INCORRECT_CATEGORY. Case does not matter, and a longer form of the
// word ("enjoyed", "logins") counts.
var bannedUtilityWords = regexp.MustCompile(`(?i)\b(password|login|username|sign[ -]in|one-time code|gate code|welcome|enjoy|thank you for choosing)`)

// bannedWordsWaitingForSwitch are approved templates with a banned word whose rewrite is in the
// manifest but whose sender still uses the old name. Remove a name once its code sends the rewrite;
// the template itself then leaves the manifest, as earlier rewrites did.
var bannedWordsWaitingForSwitch = map[string]string{}

// shortRatioApproved are templates Meta approved before the four words per variable rule was
// enforced here. Meta accepted their ratio, so a rewrite would only put a working template back into
// review (and risk a move to MARKETING) for nothing; they are held to Meta's own refusal level
// instead (more than two words per variable: three variables and six words was refused). Never add
// a name: a new template meets the rule. Remove a name when its template is rewritten.
var shortRatioApproved = map[string]bool{
	"broadcast_update_v1":                          true,
	"digitika_installment_receipt_v1_btn":          true,
	"digitika_installment_reminder_v1_btn":         true,
	"finance_payment_receipt":                      true,
	"hadia_gift_owner_alert_v1_btn":                true,
	"hadia_gift_receipt_v1_btn":                    true,
	"hadia_payout_completed_v1_btn":                true,
	"hadia_payout_part_paid_v1_btn":                true,
	"inventory_low_stock_alert":                    true,
	"ispbilling_payment_receipt":                   true,
	"ispbilling_payment_received":                  true,
	"logistics_insurance_expiring_business_v1_btn": true,
	"maskani_arrears_escalated_v1_btn":             true,
	"maskani_arrears_reminder_v1_btn":              true,
	"maskani_bill_issued_nopaybill_v1_btn":         true,
	"maskani_bill_issued_v1_btn":                   true,
	"maskani_bill_issued_vat_v1_btn":               true,
	"maskani_incident_alert_v1_btn":                true,
	"maskani_instalment_due_v1_btn":                true,
	"maskani_payment_received_v1_btn":              true,
	"maskani_resident_request_v1_btn":              true,
	"maskani_visitor_arrived_v1":                   true,
	"maskani_visitor_pass_v4":                      true,
	"ordering_new_order_business_v1_btn":           true,
	"ordering_order_for_pickup_v3_btn":             true,
	"ordering_order_out_for_delivery_v3_btn":       true,
	"ordering_order_placed_v3_btn":                 true,
	"ordering_order_refunded_v3":                   true,
	"ordering_order_scheduled_v3_btn":              true,
	"pos_payment_received_v1":                      true,
	"pos_receipt_share_btn":                        true,
	"pos_refund_processed_v1":                      true,
	"projects_milestone_approved_short_v1_btn":     true,
	"projects_milestone_approved_v1_btn":           true,
	"projects_milestone_changes_v1_btn":            true,
	"projects_task_mentioned_v1_btn":               true,
	"subscription_invoice_ready_v1_btn":            true,
}

// bodyWords counts the words a body has besides its variables: every whitespace separated token
// with a letter or digit once the {{n}} are taken out ("*{{2}}*" leaves only punctuation).
func bodyWords(body string) int {
	n := 0
	for _, f := range strings.Fields(placeholder.ReplaceAllString(body, " ")) {
		if strings.IndexFunc(f, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			n++
		}
	}
	return n
}

// TestManifestFollowsWordingPolicy checks the wording rules of docs/whatsapp-template-policy.md
// sections 3 and 3b that can be checked without Meta:
//   - a UTILITY template has none of the banned words in its body, its examples or its button text
//     (an example value counts too: Meta reads them);
//   - a body has at least four words of its own per variable (Meta error 2388293, "parameters words
//     ratio");
//   - a body that is mostly one free text variable is billed as MARKETING (meta_category), since
//     Meta treats it so whatever its wording. "Mostly" is the longest example having at least as
//     many words as the rest of the body.
func TestManifestFollowsWordingPolicy(t *testing.T) {
	defs, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	for old, rewrite := range bannedWordsWaitingForSwitch {
		if !names[old] || !names[rewrite] {
			t.Errorf("bannedWordsWaitingForSwitch: %s or its rewrite %s is not in the manifest; drop the entry", old, rewrite)
		}
	}
	for name := range shortRatioApproved {
		if !names[name] {
			t.Errorf("shortRatioApproved: %s is not in the manifest; drop the entry", name)
		}
	}

	for _, d := range defs {
		if d.Category == "AUTHENTICATION" || d.Body == "" {
			continue
		}

		if _, waiting := bannedWordsWaitingForSwitch[d.Name]; d.Category == "UTILITY" && !waiting {
			texts := append([]string{d.Body}, d.Example...)
			for _, b := range d.Buttons {
				texts = append(texts, b.Text)
			}
			for _, s := range texts {
				if w := bannedUtilityWords.FindString(s); w != "" {
					t.Errorf("%s: %q reads as a login or a promotion to Meta (policy section 3); write a _v2 without it", d.Name, w)
				}
			}
		}

		nums := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(d.Body, -1) {
			nums[m[1]] = true
		}
		vars := len(nums)
		words := bodyWords(d.Body)
		switch {
		case shortRatioApproved[d.Name]:
			if words <= 2*vars {
				t.Errorf("%s: %d words for %d variables; Meta refuses two or fewer per variable", d.Name, words, vars)
			}
			if words >= 4*vars {
				t.Errorf("%s: now has four words per variable; drop it from shortRatioApproved", d.Name)
			}
		case words < 4*vars:
			t.Errorf("%s: %d words besides its %d variables; write at least four per variable (Meta error 2388293)", d.Name, words, vars)
		}

		longest := 0
		for _, ex := range d.Example {
			longest = max(longest, len(strings.Fields(ex)))
		}
		if longest > 0 && longest >= words && d.Billed() != "MARKETING" {
			t.Errorf("%s: the body is mostly one free text variable (%d words against %d of its own), which Meta "+
				"bills as MARKETING; carry facts instead, or record \"meta_category\": \"MARKETING\"", d.Name, longest, words)
		}
	}
}
