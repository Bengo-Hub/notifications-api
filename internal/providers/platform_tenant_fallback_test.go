package providers

import (
	"context"
	"errors"
	"testing"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// The platform tenant's own rows were deleted by Disconnect on 15 November 2026 and every
// platform-scope send then failed. With no number or token of its own, the platform tenant sends
// with the platform tier credentials (Platform > Providers > WhatsApp).
func TestPlatformTenantFallsBackToPlatformTier(t *testing.T) {
	ctx := context.Background()
	for name, own := range map[string]pcfg.Settings{
		"no rows":         {},
		"number no token": {"phone_number_id": "PP", "waba_id": "PW"},
	} {
		m, reads := managerWith(own, platformMetaRow)
		p, err := m.GetWhatsAppProvider(ctx, "platform-id", "")
		if err != nil || p == nil {
			t.Fatalf("%s: provider = %v, %v", name, p, err)
		}
		if *reads == 0 {
			t.Fatalf("%s: the platform tier was never read", name)
		}
	}

	// Without a platform tier token there is still nothing to send with.
	m, _ := managerWith(pcfg.Settings{}, pcfg.Settings{"phone_number_id": "PP"})
	if _, err := m.GetWhatsAppProvider(ctx, "platform-id", ""); !errors.Is(err, ErrNoWhatsAppNumber) {
		t.Fatalf("err = %v, want ErrNoWhatsAppNumber", err)
	}
}

// The fallback is for the platform tenant only: any other tenant without its own number still gets
// ErrNoWhatsAppNumber and the platform credentials are never read for it.
func TestPlatformTierFallbackNeverReachesOtherTenants(t *testing.T) {
	m, reads := managerWith(pcfg.Settings{}, platformMetaRow)
	if _, err := m.GetWhatsAppProvider(context.Background(), "tenant-1", ""); !errors.Is(err, ErrNoWhatsAppNumber) {
		t.Fatalf("err = %v, want ErrNoWhatsAppNumber", err)
	}
	if *reads != 0 {
		t.Fatalf("platform credentials read %d times for another tenant", *reads)
	}
}

// Platform template management keeps working on a managed-link platform row (no token of its own):
// the token comes from the platform credentials, the account from the row.
func TestPlatformTemplateCredentialsUseMergedPlatformSettings(t *testing.T) {
	m, _ := managerWith(nil, pcfg.Settings{"access_token": "platform-token", "waba_id": "PW"})
	waba, token, err := m.LoadWhatsAppTemplateCredentials(context.Background())
	if err != nil || waba != "PW" || token != "platform-token" {
		t.Fatalf("got %q %q %v", waba, token, err)
	}
	m, _ = managerWith(nil, pcfg.Settings{"waba_id": "PW"})
	if _, _, err := m.LoadWhatsAppTemplateCredentials(context.Background()); err == nil {
		t.Fatal("no token anywhere must fail clearly")
	}
}
