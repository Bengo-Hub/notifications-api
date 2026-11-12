package providers

import (
	"context"
	"errors"
	"testing"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// A tenant's template sync needs its own WABA ID and token; anything missing is a clear error,
// never a quiet switch to the platform's WABA.
func TestTenantTemplateCredentials(t *testing.T) {
	cases := []struct {
		name    string
		s       pcfg.Settings
		loadErr error
		wantErr error
		waba    string
	}{
		{name: "complete", s: pcfg.Settings{"waba_id": "W1", "access_token": "tok"}, waba: "W1"},
		{name: "no waba", s: pcfg.Settings{"access_token": "tok"}, wantErr: ErrNoTenantWABA},
		{name: "nothing saved", s: pcfg.Settings{}, wantErr: ErrNoTenantWABA},
		{name: "blank waba", s: pcfg.Settings{"waba_id": "  ", "access_token": "tok"}, wantErr: ErrNoTenantWABA},
		{name: "no token", s: pcfg.Settings{"waba_id": "W1"}, wantErr: ErrNoTenantWABAToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			waba, token, err := tenantTemplateCredentials(tc.s, tc.loadErr)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if waba != "" || token != "" {
					t.Fatalf("credentials returned with an error: %q %q", waba, token)
				}
				return
			}
			if err != nil || waba != tc.waba || token != "tok" {
				t.Fatalf("got %q %q %v", waba, token, err)
			}
		})
	}
}

func TestTenantTemplateCredentialsLoadError(t *testing.T) {
	boom := errors.New("db down")
	if _, _, err := tenantTemplateCredentials(pcfg.Settings{"waba_id": "W1", "access_token": "tok"}, boom); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the load error wrapped", err)
	}
}

func TestLoadTenantWhatsAppTemplateCredentialsNeedsTenant(t *testing.T) {
	m := &Manager{PlatformID: "platform-id"}
	if _, _, err := m.LoadTenantWhatsAppTemplateCredentials(context.Background(), " "); err == nil {
		t.Fatal("an empty tenant id must be refused, not resolved to the platform")
	}
}
