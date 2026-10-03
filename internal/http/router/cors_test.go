package router

import "testing"

func TestOriginAllower(t *testing.T) {
	allow := originAllower([]string{"https://erp.masterspace.co.ke", "http://localhost:3000/"})
	cases := map[string]bool{
		"https://pos.codevertexafrica.com":       true, // was refused before
		"https://inventory.codevertexafrica.com": true,
		"https://codevertexafrica.com":           true,
		"https://erp.masterspace.co.ke":          true,  // configured custom domain
		"http://localhost:3000":                  true,  // configured, trailing slash ignored
		"http://pos.codevertexafrica.com":        false, // plain http
		"https://pos.codevertexafrica.com:8443":  false, // explicit port
		"https://evilcodevertexafrica.com":       false, // not a subdomain
		"https://codevertexafrica.com.evil.io":   false,
		"https://kuraweigh.kura.go.ke":           false, // custom domain not configured
		"null":                                   false,
		"":                                       false,
	}
	for origin, want := range cases {
		if got := allow(nil, origin); got != want {
			t.Errorf("%q: allowed=%v, want %v", origin, got, want)
		}
	}
	if !originAllower([]string{"*"})(nil, "https://anything.example") {
		t.Error("a configured * must allow every origin")
	}
}
