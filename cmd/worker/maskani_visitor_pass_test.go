package main

import (
	"strings"
	"testing"
)

// The visitor pass message reads as a booking confirmation (maskani_visitor_pass_v4): who booked the
// visit, where, when, and a grouped booking number.
func TestVisitorPassV4Params(t *testing.T) {
	h := maskaniMappings["pass.created"]
	if h.WATemplate != "maskani_visitor_pass_v4" {
		t.Fatalf("template %s", h.WATemplate)
	}
	d := h.Data(map[string]any{
		"visitor_name": "Peter", "code": "482915", "host_name": "Jane Wanjiru", "unit_code": "B07",
		"valid_from": "2026-10-11T09:00:00Z", "valid_to": "2026-10-11T21:00:00Z",
	}, &tenantInfo{Name: "Shaba Village"})
	got := h.WAParams(d)
	if len(got) != 6 || got[0] != "Peter" || got[1] != "Jane Wanjiru" || got[2] != "unit B07 at Shaba Village" || got[5] != "482 915" {
		t.Fatalf("params %v", got)
	}

	// A staff pass with no host and no unit still reads naturally.
	d = h.Data(map[string]any{"visitor_name": "Peter", "code": "482915"}, &tenantInfo{Name: "Shaba Village"})
	got = h.WAParams(d)
	if got[1] != "The Shaba Village office" || got[2] != "Shaba Village" {
		t.Fatalf("no host or unit: %v", got)
	}
	for _, p := range got {
		if strings.TrimSpace(p) == "" {
			t.Fatalf("empty parameter in %v", got)
		}
	}
}

func TestGroupedCode(t *testing.T) {
	for in, want := range map[string]string{"482915": "482 915", " 482915 ": "482 915", "12345": "12345", "": ""} {
		if got := groupedCode(in); got != want {
			t.Errorf("groupedCode(%q) = %q, want %q", in, got, want)
		}
	}
}
