package announcements

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestNormalizeVariants(t *testing.T) {
	in := Input{Title: "PayHero", Summary: "Ask us to turn it on", Metadata: map[string]any{
		MetaVariants: map[string]any{
			"payhero_active": map[string]any{
				"summary":    " PayHero is on. ",
				"highlights": []any{" Pick PayHero at checkout ", ""},
				"cta_label":  "Open settings",
				"cta_url":    "https://books.codevertexafrica.com/{orgSlug}/settings?tab=payments",
			},
		},
	}}
	if err := in.Normalize(); err != nil {
		t.Fatal(err)
	}
	// A stored row comes back from JSON as plain maps; the public read must still parse it.
	b, _ := json.Marshal(in.Metadata)
	var stored map[string]any
	_ = json.Unmarshal(b, &stored)
	v := VariantsOf(stored)["payhero_active"]
	if v.Summary != "PayHero is on." || len(v.Highlights) != 1 || v.Highlights[0] != "Pick PayHero at checkout" || v.CTALabel != "Open settings" {
		t.Fatalf("variant = %+v", v)
	}

	bad := map[string]map[string]any{
		"bad flag":          {"PayHero Active": map[string]any{"summary": "x"}},
		"no summary":        {"payhero_active": map[string]any{"title": "x"}},
		"label without url": {"payhero_active": map[string]any{"summary": "x", "cta_label": "Go"}},
		"javascript link":   {"payhero_active": map[string]any{"summary": "x", "cta_label": "Go", "cta_url": "javascript:alert(1)"}},
	}
	for name, variants := range bad {
		in := Input{Title: "t", Summary: "s", Metadata: map[string]any{MetaVariants: variants}}
		if err := in.Normalize(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if err := (&Input{Title: "t", Summary: "s", Metadata: map[string]any{MetaVariants: "nope"}}).Normalize(); !errors.Is(err, ErrInvalid) {
		t.Errorf("non-object variants: err = %v", err)
	}
}

func TestVariantsOfSkipsBadRows(t *testing.T) {
	if VariantsOf(nil) != nil || VariantsOf(map[string]any{MetaVariants: "x"}) != nil {
		t.Fatal("unreadable variants must read as none")
	}
	got := VariantsOf(map[string]any{MetaVariants: map[string]any{
		"ok":      map[string]any{"summary": "fine"},
		"Bad Key": map[string]any{"summary": "fine"},
		"empty":   map[string]any{"summary": " "},
	}})
	if len(got) != 1 || got["ok"].Highlights == nil {
		t.Fatalf("got %+v", got)
	}
}

func TestSafeLinkMailto(t *testing.T) {
	for link, want := range map[string]bool{
		"mailto:support@codevertexafrica.com?subject=Activate%20PayHero%20for%20{orgSlug}": true,
		"mailto:nobody":       false,
		"javascript:alert(1)": false,
	} {
		if got := safeLink(link); got != want {
			t.Errorf("safeLink(%q) = %v, want %v", link, got, want)
		}
	}
}
