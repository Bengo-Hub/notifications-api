package identity

import "testing"

// A user who confirms their email is updated here too; an update that does not mention
// verification leaves the stored flag alone.
func TestUserUpdatedCarriesEmailVerified(t *testing.T) {
	var ev AuthUserUpdatedEvent
	if _, err := decodeAuthPayload([]byte(`{"tenant_id":"t","payload":{"user_id":"u","email":"a@b.co","email_verified":true}}`), &ev); err != nil {
		t.Fatal(err)
	}
	if v, ok := ev.userData()["email_verified"].(bool); !ok || !v {
		t.Fatalf("email_verified must reach the sync: %+v", ev.userData())
	}

	var silent AuthUserUpdatedEvent
	if _, err := decodeAuthPayload([]byte(`{"payload":{"user_id":"u","full_name":"Bill Robinson"}}`), &silent); err != nil {
		t.Fatal(err)
	}
	if _, ok := silent.userData()["email_verified"]; ok {
		t.Fatal("an update without email_verified must not reset it")
	}
}
