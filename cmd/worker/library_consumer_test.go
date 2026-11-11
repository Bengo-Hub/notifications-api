package main

import (
	"context"
	"testing"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
)

// Library events carry no crm_contact_id yet: the member is the payload's email and name. Once
// library-api adds the id, the CRM contact wins field by field.
func TestLibraryMemberFromPayloadOrCRM(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := crmServer(t, &crm.Contact{ID: contactID, Email: "member@crm.example"})
	ti := &tenantInfo{ID: tenant}

	evt := libraryEvent{TenantID: tenant.String(), Payload: map[string]any{"email": "m@example.com", "name": "Mary"}}
	if got := libraryMember(context.Background(), res, evt, ti); got.Email != "m@example.com" || got.Name != "Mary" {
		t.Fatalf("without crm_contact_id: got %+v, want the payload", got)
	}

	evt.Payload["crm_contact_id"] = contactID.String()
	if got := libraryMember(context.Background(), res, evt, ti); got.Email != "member@crm.example" || got.Name != "Mary" {
		t.Fatalf("with crm_contact_id: got %+v, want CRM email and payload name", got)
	}
}
