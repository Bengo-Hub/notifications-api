package main

import (
	"context"
	"testing"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/contacts"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// An unlinked member's event carries the payload's email and name. A linked member's event
// carries crm_contact_id, and the CRM contact wins field by field.
func TestLibraryMemberFromPayloadOrCRM(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := crmServer(t, &crm.Contact{ID: contactID, Email: "member@crm.example"})
	ti := &tenantInfo{ID: tenant}

	evt := libraryEvent{TenantID: tenant.String(), Payload: map[string]any{"email": "m@example.com", "name": "Mary"}}
	if got, err := libraryMember(context.Background(), res, evt, ti); err != nil || got.Email != "m@example.com" || got.Name != "Mary" {
		t.Fatalf("without crm_contact_id: got %+v (%v), want the payload", got, err)
	}

	evt.Payload["crm_contact_id"] = contactID.String()
	if got, err := libraryMember(context.Background(), res, evt, ti); err != nil || got.Email != "member@crm.example" || got.Name != "Mary" {
		t.Fatalf("with crm_contact_id: got %+v (%v), want CRM email and payload name", got, err)
	}
}

// library-api sends only crm_contact_id for a linked member: the welcome goes to the CRM email
// and both the greeting and the member name come from the CRM.
func TestLibraryNoticeFromContactIDOnly(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := crmServer(t, &crm.Contact{ID: contactID, FirstName: "Mary", LastName: "Atieno", Email: "mary@crm.example"})
	ti := &tenantInfo{ID: tenant, ContactEmail: "librarian@example.com", Website: "https://lib.example"}
	evt := libraryEvent{ID: "e1", AggregateType: "library", EventType: "member.registered", TenantID: tenant.String(),
		Payload: map[string]any{"member_id": "m1", "membership_no": "LIB-1", "crm_contact_id": contactID.String()}}

	n, outcome := buildLibraryNotice(context.Background(), res, evt, ti, libraryMappings["library.member.registered"])
	if outcome != noticeSend {
		t.Fatalf("outcome %v, want send when the CRM answers", outcome)
	}
	if n.To != "mary@crm.example" || n.Target != messaging.TargetCustomer {
		t.Fatalf("to %q target %q, want the member's CRM email", n.To, n.Target)
	}
	if n.Data["name"] != "Mary Atieno" || n.Data["member_name"] != "Mary Atieno" {
		t.Fatalf("name %v member_name %v, want the CRM name", n.Data["name"], n.Data["member_name"])
	}
}

// With only the contact id and the CRM down, the event is retried rather than sent to the
// librarian in the member's place.
func TestLibraryNoticeRetriesWhenCRMDownAndOnlyID(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	res := crmServer(t, nil)
	ti := &tenantInfo{ID: tenant, ContactEmail: "librarian@example.com"}
	evt := libraryEvent{ID: "e2", AggregateType: "library", EventType: "hold.ready", TenantID: tenant.String(),
		Payload: map[string]any{"hold_id": "h1", "member_id": "m1", "crm_contact_id": "22222222-2222-2222-2222-222222222222"}}

	if _, outcome := buildLibraryNotice(context.Background(), res, evt, ti, libraryMappings["library.hold.ready"]); outcome != noticeRetry {
		t.Fatalf("outcome %v, want a retry", outcome)
	}
}

// An unlinked member without an email still routes to the librarian, as before.
func TestLibraryNoticeUnlinkedWithoutEmailGoesToLibrarian(t *testing.T) {
	res := crmServer(t, nil)
	ti := &tenantInfo{ContactEmail: "librarian@example.com"}
	evt := libraryEvent{ID: "e3", AggregateType: "library", EventType: "loan.overdue", TenantID: "t1",
		Payload: map[string]any{"loan_id": "l1", "member_id": "m1"}}

	n, outcome := buildLibraryNotice(context.Background(), res, evt, ti, libraryMappings["library.loan.overdue"])
	if outcome != noticeSend || n.To != "librarian@example.com" || n.Target != messaging.TargetStaff {
		t.Fatalf("got %+v outcome=%v, want the librarian", n, outcome)
	}
}

// A linked member's notice never goes to the librarian. With the CRM client switched off (no
// URL or key) there is no email for the member, so the event is dropped, not redirected.
func TestLibraryNoticeLinkedWithDisabledCRMIsDropped(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	res := contacts.New(crm.New("", "", nil), nil)
	ti := &tenantInfo{ID: tenant, ContactEmail: "librarian@example.com"}
	evt := libraryEvent{ID: "e4", AggregateType: "library", EventType: "loan.overdue", TenantID: tenant.String(),
		Payload: map[string]any{"loan_id": "l1", "member_id": "m1", "crm_contact_id": "22222222-2222-2222-2222-222222222222"}}

	n, outcome := buildLibraryNotice(context.Background(), res, evt, ti, libraryMappings["library.loan.overdue"])
	if outcome != noticeDrop || n.To != "" {
		t.Fatalf("got %+v outcome=%v, want the notice dropped", n, outcome)
	}
}

// A contact the CRM does not know (erased, or held under another tenant id) is dropped too.
func TestLibraryNoticeLinkedUnknownContactIsDropped(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	other := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	res := crmServer(t, &crm.Contact{ID: other, Email: "someone@crm.example"})
	ti := &tenantInfo{ID: tenant, ContactEmail: "librarian@example.com"}
	evt := libraryEvent{ID: "e5", AggregateType: "library", EventType: "fine.paid", TenantID: tenant.String(),
		Payload: map[string]any{"fine_id": "f1", "member_id": "m1", "crm_contact_id": "22222222-2222-2222-2222-222222222222"}}

	n, outcome := buildLibraryNotice(context.Background(), res, evt, ti, libraryMappings["library.fine.paid"])
	if outcome != noticeDrop || n.To != "" {
		t.Fatalf("got %+v outcome=%v, want the notice dropped", n, outcome)
	}
}
