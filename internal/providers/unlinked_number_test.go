package providers

import (
	"context"
	"errors"
	"testing"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// Hadia, 14 November 2026: a phone number ID typed into the manual field and saved, with no
// WhatsApp account, no token and no managed-link marker. Every WhatsApp call has to say the number
// still needs linking from our portfolio, not ask for an Embedded Signup reconnect.
func TestNumberSavedWithoutItsAccountIsUnlinked(t *testing.T) {
	half := pcfg.Settings{"phone_number_id": "1471029032759769"}

	if _, _, _, err := tenantProfileCredentials(half, nil); !errors.Is(err, ErrTenantNumberUnlinked) {
		t.Fatalf("profile err = %v, want ErrTenantNumberUnlinked", err)
	}
	if _, _, err := tenantTemplateCredentials(half, nil); !errors.Is(err, ErrTenantNumberUnlinked) {
		t.Fatalf("template err = %v, want ErrTenantNumberUnlinked", err)
	}

	m, reads := managerWith(half, platformMetaRow)
	_, err := m.GetWhatsAppProvider(context.Background(), "tenant-1", "")
	if !errors.Is(err, ErrNoWhatsAppNumber) || !errors.Is(err, ErrTenantNumberUnlinked) {
		t.Fatalf("send err = %v, want ErrNoWhatsAppNumber (the worker skips it) wrapping ErrTenantNumberUnlinked", err)
	}
	if *reads != 0 {
		t.Fatal("platform credentials must not be read for an unlinked number")
	}
}

// The other states keep their own answers.
func TestUnlinkedOnlyForANumberWithoutAccountOrToken(t *testing.T) {
	if _, _, err := tenantTemplateCredentials(pcfg.Settings{}, nil); !errors.Is(err, ErrNoTenantWABA) {
		t.Fatalf("nothing saved: %v, want ErrNoTenantWABA", err)
	}
	if _, _, err := tenantTemplateCredentials(pcfg.Settings{"phone_number_id": "P1", "waba_id": "W1"}, nil); !errors.Is(err, ErrNoTenantWABAToken) {
		t.Fatalf("account without token: %v, want ErrNoTenantWABAToken", err)
	}
	// A managed link whose platform token is missing is a platform problem, not an unlinked number.
	managed := pcfg.Settings{"phone_number_id": "P1", "token_source": TokenSourcePlatform}
	if _, _, _, err := tenantProfileCredentials(managed, nil); errors.Is(err, ErrTenantNumberUnlinked) || err == nil {
		t.Fatalf("managed link without platform token: %v", err)
	}
	m, _ := managerWith(pcfg.Settings{}, platformMetaRow)
	if _, err := m.GetWhatsAppProvider(context.Background(), "tenant-1", ""); !errors.Is(err, ErrNoWhatsAppNumber) || errors.Is(err, ErrTenantNumberUnlinked) {
		t.Fatalf("no number at all: %v, want plain ErrNoWhatsAppNumber", err)
	}
}
