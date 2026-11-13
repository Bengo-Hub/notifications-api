package handlers

import "testing"

// SMS delivery reports log only the last three digits of the phone (S2-8 review M-14).
func TestPhoneLast3(t *testing.T) {
	for in, want := range map[string]string{
		"+254712345678": "678",
		"0712 345 678":  "678",
		"12":            "",
		"":              "",
	} {
		if got := phoneLast3(in); got != want {
			t.Errorf("phoneLast3(%q) = %q, want %q", in, got, want)
		}
	}
}
