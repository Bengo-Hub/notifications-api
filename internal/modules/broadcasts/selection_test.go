package broadcasts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/bengobox/notifications-api/internal/ent"
	entbroadcast "github.com/bengobox/notifications-api/internal/ent/broadcast"
)

// Channels follow what is valid on file: both valid gets every channel, email only gets email,
// phone only gets one phone channel (WhatsApp before SMS), nothing valid gets nothing.
func TestPersonChannels(t *testing.T) {
	all := []string{"email", "sms", "whatsapp"}
	cases := []struct {
		name            string
		channels        []string
		hasEmail, phone bool
		want            []string
	}{
		{"both valid", all, true, true, all},
		{"email only", all, true, false, []string{"email"}},
		{"phone only prefers whatsapp", all, false, true, []string{"whatsapp"}},
		{"phone only, sms broadcast", []string{"email", "sms"}, false, true, []string{"sms"}},
		{"email only, phone broadcast", []string{"whatsapp"}, true, false, nil},
		{"nothing valid", all, false, false, nil},
	}
	for _, c := range cases {
		if got := personChannels(c.channels, c.hasEmail, c.phone); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSelectCandidatesSkipsUnreachablePeople(t *testing.T) {
	b := &ent.Broadcast{Class: entbroadcast.ClassMarketing, Metadata: map[string]any{}}
	people := []Person{
		{Key: "both", Region: "KE", Emails: []Address{{Value: "a@b.co.ke"}}, Phones: []Address{{Value: "0712345678"}}},
		{Key: "email", Region: "KE", Emails: []Address{{Value: "c@d.co.ke"}}, Phones: []Address{{Value: "12"}}},
		{Key: "phone", Region: "KE", Emails: []Address{{Value: "broken"}}, Phones: []Address{{Value: "0722000111"}}},
		{Key: "none", Region: "KE", Emails: []Address{{Value: "broken"}}, Phones: []Address{{Value: "12"}}},
	}
	got := map[string][]string{}
	for _, c := range selectCandidates(b, people, []string{"email", "whatsapp"}) {
		got[c.person.Key] = append(got[c.person.Key], c.channel)
	}
	want := map[string][]string{"both": {"email", "whatsapp"}, "email": {"email"}, "phone": {"whatsapp"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// The platform's own tenant is the sender of platform broadcasts, never a recipient, and partner
// tenants on the exclusion list are never reached either.
func TestAuthReachLeavesOutThePlatformTenant(t *testing.T) {
	const platform = "4414b8d9-6b00-4ad1-a0b4-3094cbc5e398"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"key": platform, "tenant_id": platform, "business_name": "CodeVertex Africa Limited"},
			{"key": "6f1c1f0e-0000-4000-8000-000000000001", "tenant_id": "6f1c1f0e-0000-4000-8000-000000000001", "business_name": "Urban Loft"},
			{"key": "6f1c1f0e-0000-4000-8000-000000000002", "tenant_id": "6f1c1f0e-0000-4000-8000-000000000002", "slug": "kura-hq", "business_name": "KURA"},
			{"key": "6f1c1f0e-0000-4000-8000-000000000003", "tenant_id": "6f1c1f0e-0000-4000-8000-000000000003", "slug": "mccl", "business_name": "Migori County Creameries"},
		}, "next": "6f1c1f0e-0000-4000-8000-000000000001"})
	}))
	defer srv.Close()
	a := &AuthReach{BaseURL: srv.URL, APIKey: "k", HTTP: srv.Client(), PlatformTenantID: platform, ExcludedTenants: []string{"masterspace", "kura", "mccl", "migori"}}
	people, next, err := a.Page(context.Background(), map[string]any{"type": AudiencePlatformTenants}, nil, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(people) != 1 || people[0].BusinessName != "Urban Loft" {
		t.Errorf("platform tenant and excluded partner tenants must be left out: %+v", people)
	}
	if next == "" {
		t.Error("paging cursor must still advance past a filtered page")
	}
}
