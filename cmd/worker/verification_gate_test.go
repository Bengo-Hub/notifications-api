package main

import "testing"

// The email-verification gate drops mail to unverified accounts, so every email a person needs
// in order to verify or sign in for the first time must be exempt. The Digitika portal account
// email carries the temporary password and went missing until digitika/ was exempt.
func TestVerificationExemptTemplate(t *testing.T) {
	cases := map[string]bool{
		"digitika/portal_account":       true,
		"digitika/installment_reminder": true,
		"digitika/enrollment_confirmed": true,
		"auth/otp_verification":         true,
		"auth/password_reset":           true,
		"auth/welcome":                  true,
		"auth/verify_email":             true,
		"inventory/po_received":         false,
		"treasury/invoice_sent":         false,
	}
	for template, want := range cases {
		if got := verificationExemptTemplate(template); got != want {
			t.Errorf("verificationExemptTemplate(%q) = %v, want %v", template, got, want)
		}
	}
}
