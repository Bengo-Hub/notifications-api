package main

import (
	"testing"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// A tenant's own WhatsApp never goes from the platform number (owner, 9 and 20 November 2026): a
// platform-scope message (free, never plan-gated) that a tenant sends to its own customers or
// staff, such as a sign-in code, still resolves to the tenant's number. Codevertex's own notices
// to a tenant's admins or to platform admins stay on the platform number.
func TestWhatsAppNumberTenant(t *testing.T) {
	const platform, hadia = "plat-id", "hadia-id"
	cases := []struct {
		name     string
		scope    string
		target   string
		resolved string
		want     string
	}{
		{"tenant scope uses tenant", messaging.SenderScopeTenant, messaging.TargetCustomer, hadia, hadia},
		{"platform otp to tenant customer uses tenant", messaging.SenderScopePlatform, messaging.TargetCustomer, hadia, hadia},
		{"platform notice to tenant staff uses tenant", messaging.SenderScopePlatform, messaging.TargetStaff, hadia, hadia},
		{"platform billing to tenant admin uses platform", messaging.SenderScopePlatform, messaging.TargetTenantAdmin, hadia, platform},
		{"platform ops alert uses platform", messaging.SenderScopePlatform, messaging.TargetPlatformAdmin, hadia, platform},
		{"platform tenant itself", messaging.SenderScopePlatform, messaging.TargetCustomer, platform, platform},
		{"no tenant resolved uses platform", messaging.SenderScopePlatform, messaging.TargetCustomer, "", platform},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := &messaging.Message{SenderScope: c.scope, Target: c.target}
			if got := whatsAppNumberTenant(msg, c.resolved, platform); got != c.want {
				t.Fatalf("whatsAppNumberTenant = %q, want %q", got, c.want)
			}
		})
	}
}
