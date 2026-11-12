package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/contacts"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// Every POS notice reaches its customer by email or WhatsApp: both template files exist, the
// WhatsApp template is in the manifest with exactly as many parameters as the notice sends, and
// no parameter is empty (Meta refuses the send).
func TestPosNoticesMatchTemplatesAndManifest(t *testing.T) {
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	placeholder := regexp.MustCompile(`\{\{(\d+)\}\}`)
	bodyParams := map[string]int{}
	for _, d := range defs {
		seen := map[string]bool{}
		for _, m := range placeholder.FindAllStringSubmatch(d.Body, -1) {
			seen[m[1]] = true
		}
		bodyParams[d.Name] = len(seen)
	}
	// A payload with every field empty except the contacts, so fallbacks are exercised.
	ti := &tenantInfo{Name: "Urban Loft Cafe", Country: "KE"}
	for event, n := range posNotices {
		for _, path := range []string{"email/" + n.TemplateID + ".html", "whatsapp/" + n.TemplateID + ".txt"} {
			if _, err := os.Stat(filepath.Join("..", "..", "templates", path)); err != nil {
				t.Errorf("%s: missing template %s", event, path)
			}
		}
		evt := posEvent{ID: "e1", TenantID: "t1", EventType: event, Payload: map[string]any{
			n.EmailKey: "jane@example.com", n.PhoneKey: "0712345678",
		}}
		notice, ok := posPartyNotice(evt, ti)
		if !ok {
			t.Errorf("%s: no notice built for a customer with contacts", event)
			continue
		}
		wa := notice.Parties[0].WhatsApp
		want, inManifest := bodyParams[wa.Name]
		if !inManifest {
			t.Errorf("%s: WhatsApp template %q is not in templates.json", event, wa.Name)
			continue
		}
		if len(wa.Params) != want {
			t.Errorf("%s: %q expects %d parameters, the notice sends %d", event, wa.Name, want, len(wa.Params))
		}
		for i, p := range wa.Params {
			if p == "" {
				t.Errorf("%s: parameter {{%d}} is empty", event, i+1)
			}
		}
		if got := partyContacts(notice.Parties[0]); got["email"] == "" || got["whatsapp"] == "" || got["sms"] != "" {
			t.Errorf("%s: channels %v, want email and WhatsApp only", event, got)
		}
	}
}

func TestPosNoticeNeedsACustomerContact(t *testing.T) {
	if _, ok := posPartyNotice(posEvent{EventType: "loyalty.points.earned", Payload: map[string]any{"customer_name": "Jane"}}, nil); ok {
		t.Fatal("a notice without email or phone must not be sent")
	}
	if _, ok := posPartyNotice(posEvent{EventType: "kds.waiter.called", Payload: map[string]any{"customer_phone": "0712"}}, nil); ok {
		t.Fatal("removed POS events must not be handled")
	}
}

// crmServer answers the CRM batch lookup with one contact, or 503 when contact is nil.
func crmServer(t *testing.T, contact *crm.Contact) *contacts.Resolver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contact == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []crm.Contact{*contact}})
	}))
	t.Cleanup(srv.Close)
	return contacts.New(crm.New(srv.URL, "key", nil), nil)
}

// A tier upgrade with crm_contact_id goes to the CRM contact's email and phone and greets the
// CRM name; the payload's fields are only the fallback.
func TestPosNoticeUsesCRMContact(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := crmServer(t, &crm.Contact{ID: contactID, FirstName: "Jane", LastName: "Wanjiru", Email: "jane@crm.example"})
	evt := posEvent{ID: "e1", TenantID: tenant.String(), EventType: "loyalty.tier_upgraded", Payload: map[string]any{
		"crm_contact_id": contactID.String(), "customer_name": "Old Name",
		"customer_email": "old@example.com", "customer_phone": "0712345678", "new_tier": "gold",
	}}
	ti := &tenantInfo{ID: tenant, Name: "Urban Loft Cafe", Country: "KE"}

	resolved, err := withCRMRecipient(context.Background(), res, evt, ti)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	notice, ok := posPartyNotice(resolved, ti)
	if !ok {
		t.Fatal("no notice built")
	}
	party := notice.Parties[0]
	if party.Email != "jane@crm.example" {
		t.Errorf("email = %q, want the CRM email", party.Email)
	}
	if party.Phone != "0712345678" {
		t.Errorf("phone = %q, want the payload phone (CRM has none)", party.Phone)
	}
	if notice.Data["name"] != "Jane Wanjiru" || party.WhatsApp.Params[0] != "Jane Wanjiru" {
		t.Errorf("greeting = %v / %q, want the CRM name", notice.Data["name"], party.WhatsApp.Params[0])
	}
	if evt.Payload["customer_email"] != "old@example.com" {
		t.Error("the original event payload must not be changed")
	}
}

// A CRM outage still sends, to the payload's own contact details.
func TestPosNoticeFallsBackWhenCRMDown(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	res := crmServer(t, nil)
	evt := posEvent{ID: "e1", TenantID: tenant.String(), EventType: "loyalty.referral_earned", Payload: map[string]any{
		"crm_contact_id": "22222222-2222-2222-2222-222222222222", "referrer_name": "Ann",
		"referrer_email": "ann@example.com", "referrer_phone": "0711111111",
	}}
	ti := &tenantInfo{ID: tenant, Name: "Urban Loft Cafe"}

	notice, ok, retry := posNoticeFor(context.Background(), res, evt, ti)
	if !ok || retry {
		t.Fatal("an outage must not drop or delay a notice that has payload contacts")
	}
	if p := notice.Parties[0]; p.Email != "ann@example.com" || p.Phone != "0711111111" || notice.Data["name"] != "Ann" {
		t.Errorf("got email %q phone %q name %v, want the payload's", p.Email, p.Phone, notice.Data["name"])
	}
}

// Events without crm_contact_id are left exactly as they were.
func TestPosNoticeWithoutCRMContactIsUnchanged(t *testing.T) {
	res := crmServer(t, nil)
	evt := posEvent{EventType: "loyalty.points.earned", Payload: map[string]any{"customer_email": "a@example.com"}}
	if got, _ := withCRMRecipient(context.Background(), res, evt, nil); len(got.Payload) != 1 || got.Payload["customer_email"] != "a@example.com" {
		t.Fatalf("payload changed: %v", got.Payload)
	}
}

func TestPosPayloadHelpers(t *testing.T) {
	p := map[string]any{"amount": "1500.50", "n": float64(3), "d": "2026-11-02", "ts": "2026-11-02T10:00:00Z"}
	if payNum(p, "amount") != 1500.5 || payInt(p, "n") != 3 {
		t.Fatal("numbers from strings and JSON numbers")
	}
	if payDate(p, "d") != "2 Nov 2026" || payDate(p, "ts") != "2 Nov 2026" {
		t.Fatalf("dates: %q %q", payDate(p, "d"), payDate(p, "ts"))
	}
	if humanize("gold_plus") != "Gold plus" || nonEmptyOr(" ", "x") != "x" {
		t.Fatal("humanize and nonEmptyOr")
	}
}

// pos-api sends only crm_contact_id (no name, email or phone) for a customer linked to the CRM:
// the notice still reaches them, with every detail from the CRM.
func TestPosNoticeFromContactIDOnly(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := crmServer(t, &crm.Contact{ID: contactID, FirstName: "Jane", LastName: "Wanjiru", Email: "jane@crm.example", Phone: "+254712345678"})
	evt := posEvent{ID: "e1", TenantID: tenant.String(), EventType: "loyalty.points.earned", Payload: map[string]any{
		"crm_contact_id": contactID.String(), "account_id": "a1", "points_earned": float64(10), "balance_after": float64(50),
	}}
	ti := &tenantInfo{ID: tenant, Name: "Urban Loft Cafe"}

	notice, ok, retry := posNoticeFor(context.Background(), res, evt, ti)
	if !ok || retry {
		t.Fatalf("ok=%v retry=%v, want a notice", ok, retry)
	}
	p := notice.Parties[0]
	if p.Email != "jane@crm.example" || p.Phone != "+254712345678" || notice.Data["name"] != "Jane Wanjiru" {
		t.Errorf("got email %q phone %q name %v, want the CRM contact", p.Email, p.Phone, notice.Data["name"])
	}
}

// The referral notice goes to the referrer: crm_contact_id on loyalty.referral_earned is the
// referrer's contact, and its details fill the referrer fields the notice reads.
func TestPosReferralNoticeUsesReferrerContact(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	referrer := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	res := crmServer(t, &crm.Contact{ID: referrer, FirstName: "Ann", Email: "ann@crm.example", Phone: "+254711111111"})
	evt := posEvent{ID: "e2", TenantID: tenant.String(), EventType: "loyalty.referral_earned", Payload: map[string]any{
		"crm_contact_id": referrer.String(), "referrer_account": "a1", "bonus_points": float64(20), "balance_after": float64(70),
	}}
	ti := &tenantInfo{ID: tenant, Name: "Urban Loft Cafe"}

	notice, ok, _ := posNoticeFor(context.Background(), res, evt, ti)
	if !ok {
		t.Fatal("no notice built")
	}
	if p := notice.Parties[0]; p.Email != "ann@crm.example" || p.WhatsApp.Params[0] != "Ann" {
		t.Errorf("got email %q greeting %q, want the referrer's CRM details", p.Email, p.WhatsApp.Params[0])
	}
}

// With only the contact id on the event and the CRM down, there is nobody to send to yet: the
// event is retried rather than dropped.
func TestPosNoticeRetriesWhenCRMDownAndOnlyID(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	res := crmServer(t, nil)
	evt := posEvent{ID: "e3", TenantID: tenant.String(), EventType: "hotel.check_in", Payload: map[string]any{
		"crm_contact_id": "22222222-2222-2222-2222-222222222222", "guest_id": "g1", "room_number": "12",
	}}
	ti := &tenantInfo{ID: tenant, Name: "Urban Loft Cafe"}

	if _, ok, retry := posNoticeFor(context.Background(), res, evt, ti); ok || !retry {
		t.Fatalf("ok=%v retry=%v, want a retry", ok, retry)
	}
}

// An unlinked customer with no contact details has nobody to reach: no notice and no retry.
func TestPosNoticeWithoutIDOrContactsIsNotRetried(t *testing.T) {
	res := crmServer(t, nil)
	evt := posEvent{ID: "e4", TenantID: "t1", EventType: "loyalty.points.earned", Payload: map[string]any{"points_earned": float64(5)}}
	if _, ok, retry := posNoticeFor(context.Background(), res, evt, nil); ok || retry {
		t.Fatalf("ok=%v retry=%v, want neither", ok, retry)
	}
}

// A linked customer with no details anywhere (CRM client switched off, or a contact the CRM
// does not know) is dropped, not retried: retrying would not change the answer.
func TestPosNoticeLinkedWithoutDetailsIsDropped(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	ti := &tenantInfo{ID: tenant, Name: "Urban Loft Cafe"}
	evt := posEvent{ID: "e5", TenantID: tenant.String(), EventType: "loyalty.tier_upgraded", Payload: map[string]any{
		"crm_contact_id": "22222222-2222-2222-2222-222222222222", "new_tier": "gold",
	}}

	disabled := contacts.New(crm.New("", "", nil), nil)
	if _, ok, retry := posNoticeFor(context.Background(), disabled, evt, ti); ok || retry {
		t.Fatalf("disabled client: ok=%v retry=%v, want a drop", ok, retry)
	}
	unknown := crmServer(t, &crm.Contact{ID: uuid.MustParse("99999999-9999-9999-9999-999999999999"), Email: "x@crm.example"})
	if _, ok, retry := posNoticeFor(context.Background(), unknown, evt, ti); ok || retry {
		t.Fatalf("unknown contact: ok=%v retry=%v, want a drop", ok, retry)
	}
}

// pos-api now sends layaway.payment_due and payment.received_notification for a CRM-linked
// customer with the record ids and crm_contact_id only: both notices still reach the customer,
// with every detail from the CRM.
func TestPosLayawayAndPaymentNoticesFromContactIDOnly(t *testing.T) {
	tenant := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	res := crmServer(t, &crm.Contact{ID: contactID, FirstName: "Jane", Email: "jane@crm.example", Phone: "+254712345678"})
	ti := &tenantInfo{ID: tenant, Name: "Urban Loft Cafe"}
	for _, evt := range []posEvent{
		{ID: "e6", TenantID: tenant.String(), EventType: "layaway.payment_due", Payload: map[string]any{
			"crm_contact_id": contactID.String(), "layaway_plan_id": "p1", "balance_due": "90", "due_date": "2026-11-13",
		}},
		{ID: "e7", TenantID: tenant.String(), EventType: "payment.received_notification", Payload: map[string]any{
			"crm_contact_id": contactID.String(), "order_id": "o1", "payment_id": "pay1", "amount": float64(100), "currency": "KES",
		}},
	} {
		notice, ok, retry := posNoticeFor(context.Background(), res, evt, ti)
		if !ok || retry {
			t.Fatalf("%s: ok=%v retry=%v, want a notice", evt.EventType, ok, retry)
		}
		if p := notice.Parties[0]; p.Email != "jane@crm.example" || p.Phone != "+254712345678" || p.WhatsApp.Params[0] != "Jane" {
			t.Errorf("%s: got email %q phone %q greeting %q, want the CRM contact", evt.EventType, p.Email, p.Phone, p.WhatsApp.Params[0])
		}
	}
}
