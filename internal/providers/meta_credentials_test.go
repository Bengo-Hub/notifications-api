package providers

import (
	"testing"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// The platform providers page saves the app ID and secret on the "platform" tier while the access
// token lives on the platform tenant's own row; Embedded Signup needs all of them together.
func TestMergeMetaCredentials(t *testing.T) {
	tier := pcfg.Settings{"app_id": "A", "app_secret": "S", "access_token": "old", "api_version": "v20.0"}
	own := pcfg.Settings{"access_token": "T", "phone_number_id": "P", "api_version": ""}
	got := mergeMetaCredentials(tier, own)
	want := map[string]string{"app_id": "A", "app_secret": "S", "access_token": "T", "phone_number_id": "P", "api_version": "v20.0"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(mergeMetaCredentials(nil, nil)) != 0 {
		t.Error("empty inputs should merge to empty")
	}
}
