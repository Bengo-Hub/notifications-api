package identity

import (
	"reflect"
	"testing"
)

func TestRawAuthRolesReadsTypedAndDecodedPayloads(t *testing.T) {
	typed := map[string]interface{}{"roles": []string{"Cashier", "customer", "cashier", " "}}
	if got, want := rawAuthRoles(typed), []string{"cashier", "customer"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("typed roles = %v, want %v", got, want)
	}
	decoded := map[string]interface{}{"roles": []interface{}{"student", 7, "parent"}}
	if got, want := rawAuthRoles(decoded), []string{"parent", "student"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded roles = %v, want %v", got, want)
	}
	if got := rawAuthRoles(map[string]interface{}{}); len(got) != 0 {
		t.Fatalf("no roles key = %v, want none", got)
	}
}

func TestWithAuthRolesNeverErasesWhatIsRecorded(t *testing.T) {
	meta := withAuthRoles(nil, map[string]interface{}{"roles": []string{"admin"}})
	if got := meta[MetaAuthRoles]; !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("recorded roles = %v", got)
	}
	// An event without roles (a profile update) leaves the record alone.
	same := withAuthRoles(meta, map[string]interface{}{"full_name": "A"})
	if got := same[MetaAuthRoles]; !reflect.DeepEqual(got, []string{"admin"}) {
		t.Fatalf("roles erased by a roleless event: %v", got)
	}
	// An event without roles on a fresh row records nothing, so the account is not gated.
	if got := withAuthRoles(nil, map[string]interface{}{}); got != nil {
		t.Fatalf("expected nil metadata, got %v", got)
	}
}

func TestCustomerFacingRolesMatchAuthSelfSignup(t *testing.T) {
	got := CustomerFacingRoles()
	want := []string{"customer", "driver", "member", "parent", "student"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("customer-facing roles = %v, want %v (keep in step with auth-api selfServiceRoles)", got, want)
	}
	got[0] = "tampered"
	if CustomerFacingRoles()[0] != "customer" {
		t.Fatal("CustomerFacingRoles must return a copy")
	}
}
