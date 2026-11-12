package providers

import (
	"context"
	"errors"
	"testing"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// The business profile is edited on the tenant's own number with the tenant's own token; there is
// never a fallback to the platform's number.
func TestTenantProfileCredentials(t *testing.T) {
	cases := []struct {
		name    string
		s       pcfg.Settings
		wantErr error
	}{
		{name: "complete", s: pcfg.Settings{"phone_number_id": "P1", "access_token": "tok", "api_version": "v26.0"}},
		{name: "no number", s: pcfg.Settings{"access_token": "tok"}, wantErr: ErrNoTenantPhoneNumber},
		{name: "blank number", s: pcfg.Settings{"phone_number_id": " ", "access_token": "tok"}, wantErr: ErrNoTenantPhoneNumber},
		{name: "no token", s: pcfg.Settings{"phone_number_id": "P1"}, wantErr: ErrNoTenantWABAToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, token, ver, err := tenantProfileCredentials(tc.s, nil)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) || id != "" || token != "" {
					t.Fatalf("got %q %q %v, want %v", id, token, err, tc.wantErr)
				}
				return
			}
			if err != nil || id != "P1" || token != "tok" || ver != "v26.0" {
				t.Fatalf("got %q %q %q %v", id, token, ver, err)
			}
		})
	}
	boom := errors.New("db down")
	if _, _, _, err := tenantProfileCredentials(pcfg.Settings{"phone_number_id": "P1", "access_token": "t"}, boom); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the load error wrapped", err)
	}
}

func TestLoadTenantWhatsAppProfileCredentialsNeedsTenant(t *testing.T) {
	m := &Manager{PlatformID: "platform-id"}
	if _, _, _, err := m.LoadTenantWhatsAppProfileCredentials(context.Background(), ""); err == nil {
		t.Fatal("an empty tenant id must be refused, not resolved to the platform")
	}
}
