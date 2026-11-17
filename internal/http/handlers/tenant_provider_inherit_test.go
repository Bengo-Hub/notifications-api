package handlers

import (
	"reflect"
	"testing"
)

var platformSMTPRows = []settingRow{
	{Key: "host", Value: "mail.codevertexafrica.com"},
	{Key: "port", Value: "465"},
	{Key: "username", Value: "no-reply@codevertexafrica.com"},
	{Key: "password", Value: "enc:abc", Secret: true},
	{Key: "from", Value: "Codevertex <no-reply@codevertexafrica.com>"},
	{Key: "reply_to", Value: "info@codevertexafrica.com"},
}

// The codevertex tenant holds only _preferred=smtp: a platform owner sees the platform's SMTP
// account in force, never its password.
func TestInheritedSMTPForPlatformOwner(t *testing.T) {
	got := inheritedProviderSettings("email", "smtp", nil, platformSMTPRows, true)
	if got.From != "platform" {
		t.Fatalf("from = %q, want platform", got.From)
	}
	want := map[string]string{
		"host": "mail.codevertexafrica.com", "port": "465", "username": "no-reply@codevertexafrica.com",
		"from": "Codevertex <no-reply@codevertexafrica.com>", "reply_to": "info@codevertexafrica.com",
	}
	if !reflect.DeepEqual(got.Effective, want) {
		t.Fatalf("effective = %v, want %v", got.Effective, want)
	}
	if !reflect.DeepEqual(got.Keys, []string{"from", "host", "port", "reply_to", "username"}) {
		t.Fatalf("keys = %v", got.Keys)
	}
}

// Another tenant learns that it uses the platform's account and sees only the sender values,
// with its own sender address on top; the platform's host and username stay hidden.
func TestInheritedSMTPForTenantShowsOnlySenderValues(t *testing.T) {
	own := []settingRow{{Key: "from", Value: "Hadia <no-reply@hadiafrica.com>"}}
	got := inheritedProviderSettings("email", "smtp", own, platformSMTPRows, false)
	want := map[string]string{"from": "Hadia <no-reply@hadiafrica.com>", "reply_to": "info@codevertexafrica.com"}
	if got.From != "platform" || !reflect.DeepEqual(got.Effective, want) {
		t.Fatalf("got %+v, want effective %v", got, want)
	}
	if !reflect.DeepEqual(got.Keys, []string{"reply_to"}) {
		t.Fatalf("keys = %v, want only reply_to from the platform", got.Keys)
	}
}

// A tenant with its own host owns the account: nothing is inherited, unless a platform-managed
// host overrides it.
func TestOwnAccountIsNotInherited(t *testing.T) {
	own := []settingRow{{Key: "host", Value: "smtp.zoho.com"}, {Key: "username", Value: "a@b.co"}}
	if got := inheritedProviderSettings("email", "smtp", own, platformSMTPRows, true); got.From != "" {
		t.Fatalf("own account reported as inherited: %+v", got)
	}
	forced := append([]settingRow{{Key: "host", Value: "mail.codevertexafrica.com", Managed: true}}, platformSMTPRows[1:]...)
	if got := inheritedProviderSettings("email", "smtp", own, forced, true); got.From != "platform" {
		t.Fatalf("a platform-managed host is in force: %+v", got)
	}
}

// Nothing saved at platform level, WhatsApp, or an unknown provider: no inheritance.
func TestNothingToInherit(t *testing.T) {
	if got := inheritedProviderSettings("email", "smtp", nil, nil, true); got.From != "" {
		t.Fatalf("got %+v", got)
	}
	if got := inheritedProviderSettings("whatsapp", "meta_cloud", nil, []settingRow{{Key: "phone_number_id", Value: "1"}}, true); got.From != "" {
		t.Fatalf("whatsapp never inherits: %+v", got)
	}
	// Only a key at platform level still says the account is the platform's, with nothing to show.
	got := inheritedProviderSettings("sms", "africastalking", nil, []settingRow{{Key: "api_key", Value: "k", Secret: true}}, false)
	if got.From != "platform" || len(got.Effective) != 0 {
		t.Fatalf("got %+v", got)
	}
}
