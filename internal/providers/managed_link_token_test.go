package providers

import (
	"context"
	"errors"
	"testing"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// managerWith returns a Manager whose tenant and platform WhatsApp settings come from memory.
func managerWith(tenant, platform pcfg.Settings) (*Manager, *int) {
	platformReads := 0
	m := &Manager{PlatformID: "platform-id"}
	m.tenantOnlyWhatsApp = func(context.Context, string) (pcfg.Settings, error) { return tenant, nil }
	m.platformMeta = func(context.Context) (pcfg.Settings, error) {
		platformReads++
		return platform, nil
	}
	return m, &platformReads
}

var platformMetaRow = pcfg.Settings{"access_token": "platform-token", "api_version": "v26.0", "waba_id": "PW", "phone_number_id": "PP"}

// A managed link (token_source=platform) sends, syncs templates and edits the profile with the
// platform system user's token on the tenant's own number and account.
func TestManagedLinkUsesPlatformToken(t *testing.T) {
	tenant := pcfg.Settings{"phone_number_id": "P1", "waba_id": "W1", "token_source": "platform", "access_token": "stale-tenant-token"}
	m, _ := managerWith(tenant, platformMetaRow)
	ctx := context.Background()

	s, err := m.tenantWhatsAppSettings(ctx, "tenant-1")
	if err != nil || s["access_token"] != "platform-token" || s["phone_number_id"] != "P1" || s["api_version"] != "v26.0" {
		t.Fatalf("settings = %v, %v", s, err)
	}
	if tenant["access_token"] != "stale-tenant-token" {
		t.Fatal("the loaded tenant settings must not be changed in place")
	}
	if p, err := m.GetWhatsAppProvider(ctx, "tenant-1", ""); err != nil || p == nil {
		t.Fatalf("provider = %v, %v", p, err)
	}
	waba, token, err := m.LoadTenantWhatsAppTemplateCredentials(ctx, "tenant-1")
	if err != nil || waba != "W1" || token != "platform-token" {
		t.Fatalf("template credentials = %q %q %v", waba, token, err)
	}
	phone, token, ver, err := m.LoadTenantWhatsAppProfileCredentials(ctx, "tenant-1")
	if err != nil || phone != "P1" || token != "platform-token" || ver != "v26.0" {
		t.Fatalf("profile credentials = %q %q %q %v", phone, token, ver, err)
	}
}

// Without the marker the tenant's own token is used and the platform's is never read, so a tenant
// number cannot send with another account's token by accident.
func TestNoMarkerKeepsTenantToken(t *testing.T) {
	m, reads := managerWith(pcfg.Settings{"phone_number_id": "P1", "waba_id": "W1", "access_token": "own-token"}, platformMetaRow)
	ctx := context.Background()
	_, token, err := m.LoadTenantWhatsAppTemplateCredentials(ctx, "tenant-1")
	if err != nil || token != "own-token" {
		t.Fatalf("template token = %q %v", token, err)
	}
	_, token, _, err = m.LoadTenantWhatsAppProfileCredentials(ctx, "tenant-1")
	if err != nil || token != "own-token" {
		t.Fatalf("profile token = %q %v", token, err)
	}
	if *reads != 0 {
		t.Fatalf("platform credentials read %d times without the marker", *reads)
	}

	// A number saved by hand with no token and no marker gets nothing, not the platform's token.
	m, reads = managerWith(pcfg.Settings{"phone_number_id": "P1", "waba_id": "W1"}, platformMetaRow)
	if _, err := m.GetWhatsAppProvider(ctx, "tenant-1", ""); !errors.Is(err, ErrNoWhatsAppNumber) {
		t.Fatalf("err = %v, want ErrNoWhatsAppNumber", err)
	}
	if _, _, err := m.LoadTenantWhatsAppTemplateCredentials(ctx, "tenant-1"); !errors.Is(err, ErrNoTenantWABAToken) {
		t.Fatalf("err = %v, want ErrNoTenantWABAToken", err)
	}
	if *reads != 0 {
		t.Fatal("platform credentials read without the marker")
	}
	// Any other marker value is not the platform marker.
	m, _ = managerWith(pcfg.Settings{"phone_number_id": "P1", "token_source": "tenant"}, platformMetaRow)
	if _, err := m.GetWhatsAppProvider(ctx, "tenant-1", ""); !errors.Is(err, ErrNoWhatsAppNumber) {
		t.Fatalf("err = %v, want ErrNoWhatsAppNumber", err)
	}
}

// A managed link with no platform token configured fails clearly instead of sending unsigned.
func TestManagedLinkWithoutPlatformToken(t *testing.T) {
	m, _ := managerWith(pcfg.Settings{"phone_number_id": "P1", "waba_id": "W1", "token_source": "platform", "access_token": "stale"}, pcfg.Settings{})
	if _, err := m.GetWhatsAppProvider(context.Background(), "tenant-1", ""); !errors.Is(err, ErrNoWhatsAppNumber) {
		t.Fatalf("err = %v, want ErrNoWhatsAppNumber", err)
	}
	if _, _, err := m.LoadTenantWhatsAppTemplateCredentials(context.Background(), "tenant-1"); !errors.Is(err, ErrNoTenantWABAToken) {
		t.Fatalf("err = %v, want ErrNoTenantWABAToken", err)
	}
}

func TestApplyPlatformTokenKeepsTenantVersion(t *testing.T) {
	got := applyPlatformToken(pcfg.Settings{"phone_number_id": "P1", "api_version": "v25.0"}, platformMetaRow)
	if got["api_version"] != "v25.0" || got["access_token"] != "platform-token" || got["phone_number_id"] != "P1" {
		t.Fatalf("got %v", got)
	}
}
