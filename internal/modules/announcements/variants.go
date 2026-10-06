package announcements

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// MetaVariants is the metadata key holding an announcement's variants.
const MetaVariants = "variants"

// flagPattern is what a variant's flag (the key an app reports) may look like.
var flagPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)

// Variant replaces an announcement's text for viewers whose app reports its flag as true, so one
// notice can read "how to ask for it" to tenants without a feature and "how to use it" to tenants
// who have it. Stored in metadata (MetaVariants, keyed by flag); the app decides the flag (for
// example payhero_active from the tenant's gateway status), and the base text is what everyone
// else sees.
type Variant struct {
	Title      string   `json:"title,omitempty"`
	Summary    string   `json:"summary"`
	Highlights []string `json:"highlights"`
	CTALabel   string   `json:"cta_label,omitempty"`
	CTAURL     string   `json:"cta_url,omitempty"`
}

// VariantsOf reads the variants stored on an announcement. Anything unreadable is left out, so a
// bad row can never break the public read.
func VariantsOf(meta map[string]any) map[string]Variant {
	raw, ok := meta[MetaVariants]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out map[string]Variant
	if json.Unmarshal(b, &out) != nil || len(out) == 0 {
		return nil
	}
	for flag, v := range out {
		if !flagPattern.MatchString(flag) || strings.TrimSpace(v.Summary) == "" {
			delete(out, flag)
			continue
		}
		if v.Highlights == nil {
			v.Highlights = []string{}
			out[flag] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeVariants validates metadata variants in place: flag names, required summary, CTA
// label and link together and safe, trimmed text.
func normalizeVariants(meta map[string]any) error {
	raw, ok := meta[MetaVariants]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("%w: variants must be an object keyed by flag", ErrInvalid)
	}
	var in map[string]Variant
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("%w: variants must be an object keyed by flag", ErrInvalid)
	}
	flags := make([]string, 0, len(in))
	for flag := range in {
		flags = append(flags, flag)
	}
	sort.Strings(flags)
	out := make(map[string]Variant, len(in))
	for _, flag := range flags {
		v := in[flag]
		if !flagPattern.MatchString(flag) {
			return fmt.Errorf("%w: variant flag %q must be lower case letters, digits and _", ErrInvalid, flag)
		}
		v.Title = strings.TrimSpace(v.Title)
		v.Summary = strings.TrimSpace(v.Summary)
		v.CTALabel = strings.TrimSpace(v.CTALabel)
		v.CTAURL = strings.TrimSpace(v.CTAURL)
		if v.Summary == "" {
			return fmt.Errorf("%w: variant %q needs a summary", ErrInvalid, flag)
		}
		if len(v.Title) > 120 {
			return fmt.Errorf("%w: variant %q title is at most 120 characters", ErrInvalid, flag)
		}
		if (v.CTALabel == "") != (v.CTAURL == "") {
			return fmt.Errorf("%w: variant %q cta_label and cta_url go together", ErrInvalid, flag)
		}
		if v.CTAURL != "" && !safeLink(v.CTAURL) {
			return fmt.Errorf("%w: variant %q cta_url must be an https URL, a mailto address or an app path", ErrInvalid, flag)
		}
		highlights := make([]string, 0, len(v.Highlights))
		for _, h := range v.Highlights {
			if h = strings.TrimSpace(h); h != "" {
				highlights = append(highlights, h)
			}
		}
		v.Highlights = highlights
		out[flag] = v
	}
	if len(out) == 0 {
		delete(meta, MetaVariants)
		return nil
	}
	meta[MetaVariants] = out
	return nil
}
