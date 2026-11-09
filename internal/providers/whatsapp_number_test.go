package providers

import (
	"testing"

	"github.com/bengobox/notifications-api/internal/config"
)

// Only allowlisted tenants (the demo) may send their own WhatsApp from the platform's number; every
// client tenant needs its own (owner, 9 November 2026).
func TestSharesPlatformNumber(t *testing.T) {
	m := &Manager{cfg: config.ProviderConfig{WhatsAppSharedNumberTenants: []string{" 982FFB25-0e07-4bbb-8e04-c4c2af2550b6 "}}}
	if !m.sharesPlatformNumber("982ffb25-0e07-4bbb-8e04-c4c2af2550b6") {
		t.Fatal("the demo tenant lost the shared number")
	}
	if m.sharesPlatformNumber("1e32db34-b65d-4384-9239-a22ef6e35985") {
		t.Fatal("a client tenant borrowed the platform number")
	}
	if (&Manager{}).sharesPlatformNumber("982ffb25-0e07-4bbb-8e04-c4c2af2550b6") {
		t.Fatal("an empty allowlist must share with nobody")
	}
}
