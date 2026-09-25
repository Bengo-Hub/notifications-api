package templatesync

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	placeholder  = regexp.MustCompile(`\{\{(\d+)\}\}`)
	templateName = regexp.MustCompile(`^[a-z0-9_]{1,512}$`)
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
		body := strings.TrimSpace(d.Body)
		if strings.HasPrefix(body, "{{") {
			t.Errorf("%s: body must not start with a variable", d.Name)
		}
		if strings.HasSuffix(body, "}}") {
			t.Errorf("%s: body must not end with a variable", d.Name)
		}

		for _, b := range d.Buttons {
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
		}
	}
}
