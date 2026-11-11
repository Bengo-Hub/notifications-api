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

	resolved := withCRMRecipient(context.Background(), res, evt, ti)
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

	notice, ok := posPartyNotice(withCRMRecipient(context.Background(), res, evt, ti), ti)
	if !ok {
		t.Fatal("an outage must not drop the notice")
	}
	if p := notice.Parties[0]; p.Email != "ann@example.com" || p.Phone != "0711111111" || notice.Data["name"] != "Ann" {
		t.Errorf("got email %q phone %q name %v, want the payload's", p.Email, p.Phone, notice.Data["name"])
	}
}

// Events without crm_contact_id are left exactly as they were.
func TestPosNoticeWithoutCRMContactIsUnchanged(t *testing.T) {
	res := crmServer(t, nil)
	evt := posEvent{EventType: "loyalty.points.earned", Payload: map[string]any{"customer_email": "a@example.com"}}
	if got := withCRMRecipient(context.Background(), res, evt, nil); len(got.Payload) != 1 || got.Payload["customer_email"] != "a@example.com" {
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
