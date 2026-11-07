package main

import "testing"

// Staging links keep their path; other hosts give none; email links follow the tenant's app URL.
func TestHadiaLinks(t *testing.T) {
	if got := hadiaPath("https://staging.hadiafrica.com/r/AB12/reserved/tok"); got != "r/AB12/reserved/tok" {
		t.Fatalf("staging path: %q", got)
	}
	if got := hadiaPath("https://hadiafrica.com.evil.example/r/AB12"); got != "" {
		t.Fatalf("lookalike host must give no path: %q", got)
	}
	if got := hadiaEmailBase(&tenantInfo{AppURL: "https://staging.hadiafrica.com/"}); got != "https://staging.hadiafrica.com" {
		t.Fatalf("app url: %q", got)
	}
	if got := hadiaEmailBase(&tenantInfo{ServiceURLs: map[string]string{"hadia": "https://x.hadiafrica.com"}, AppURL: "https://y"}); got != "https://x.hadiafrica.com" {
		t.Fatalf("service url: %q", got)
	}
	if got := hadiaEmailBase(nil); got != hadiaButtonBase {
		t.Fatalf("default: %q", got)
	}
}
