package contacts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bengo-Hub/shared-service-client/crm"
	"github.com/google/uuid"
)

var (
	tenantID  = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	contactID = uuid.MustParse("22222222-2222-2222-2222-222222222222")
)

// fakeCRM serves the batch endpoint with the given contacts and counts the calls it gets.
func fakeCRM(t *testing.T, status int, known ...crm.Contact) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.URL.Path != "/api/v1/internal/contacts/batch" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"down","code":"unavailable"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": known})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func payloadRecipient() Recipient {
	return Recipient{Name: "Payload Name", Email: "payload@example.com", Phone: "0700000000"}
}

func TestResolvePrefersCRM(t *testing.T) {
	srv, _ := fakeCRM(t, http.StatusOK, crm.Contact{
		ID: contactID, FirstName: "Jane", LastName: "Wanjiru", Email: "jane@example.com", Phone: "+254712345678",
	})
	r := New(crm.New(srv.URL, "key", nil), nil)

	got := r.Resolve(context.Background(), tenantID, contactID, payloadRecipient())
	want := Recipient{Name: "Jane Wanjiru", Email: "jane@example.com", Phone: "+254712345678"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestResolveFallsBackOnOutage(t *testing.T) {
	srv, calls := fakeCRM(t, http.StatusServiceUnavailable)
	r := New(crm.New(srv.URL, "key", nil), nil)

	if got := r.Resolve(context.Background(), tenantID, contactID, payloadRecipient()); got != payloadRecipient() {
		t.Fatalf("outage: got %+v, want the payload recipient", got)
	}
	// A failed lookup is not cached, so the next event tries the CRM again.
	r.Resolve(context.Background(), tenantID, contactID, payloadRecipient())
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("CRM calls = %d, want 2 (failures are not cached)", n)
	}

	// An unreachable CRM behaves the same.
	srv.Close()
	if got := r.Resolve(context.Background(), tenantID, contactID, payloadRecipient()); got != payloadRecipient() {
		t.Fatalf("unreachable: got %+v, want the payload recipient", got)
	}

	// A disabled client and missing ids never call out.
	disabled := New(crm.New("", "", nil), nil)
	if got := disabled.Resolve(context.Background(), tenantID, contactID, payloadRecipient()); got != payloadRecipient() {
		t.Fatalf("disabled: got %+v", got)
	}
	if got := r.Resolve(context.Background(), tenantID, uuid.Nil, payloadRecipient()); got != payloadRecipient() {
		t.Fatalf("no contact id: got %+v", got)
	}
	var nilResolver *Resolver
	if got := nilResolver.Resolve(context.Background(), tenantID, contactID, payloadRecipient()); got != payloadRecipient() {
		t.Fatalf("nil resolver: got %+v", got)
	}
}

func TestResolveCaches(t *testing.T) {
	srv, calls := fakeCRM(t, http.StatusOK, crm.Contact{ID: contactID, FirstName: "Jane", Email: "jane@example.com"})
	r := New(crm.New(srv.URL, "key", nil), nil)
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }

	first := r.Resolve(context.Background(), tenantID, contactID, Recipient{})
	second := r.Resolve(context.Background(), tenantID, contactID, Recipient{})
	if first != second || first.Email != "jane@example.com" {
		t.Fatalf("first %+v, second %+v", first, second)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("CRM calls = %d, want 1 (second call served from cache)", n)
	}

	// The cache is keyed by tenant too: another tenant's lookup goes to the CRM.
	other := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	r.Resolve(context.Background(), other, contactID, Recipient{})
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("CRM calls = %d, want 2 after a different tenant", n)
	}

	// After the TTL the contact is fetched again.
	now = now.Add(DefaultTTL + time.Second)
	r.Resolve(context.Background(), tenantID, contactID, Recipient{})
	if n := atomic.LoadInt32(calls); n != 3 {
		t.Fatalf("CRM calls = %d, want 3 after the TTL", n)
	}
}

func TestResolveFieldFallback(t *testing.T) {
	// The CRM has an email but no phone or name: those come from the payload.
	srv, _ := fakeCRM(t, http.StatusOK, crm.Contact{ID: contactID, Email: "crm@example.com"})
	r := New(crm.New(srv.URL, "key", nil), nil)

	got := r.Resolve(context.Background(), tenantID, contactID, payloadRecipient())
	want := Recipient{Name: "Payload Name", Email: "crm@example.com", Phone: "0700000000"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// A contact the CRM does not know leaves every field to the payload.
	unknown := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	if got := r.Resolve(context.Background(), tenantID, unknown, payloadRecipient()); got != payloadRecipient() {
		t.Fatalf("unknown contact: got %+v", got)
	}
}

func TestCacheIsBounded(t *testing.T) {
	srv, _ := fakeCRM(t, http.StatusOK)
	r := newResolver(crm.New(srv.URL, "key", nil), nil, DefaultTTL, 3)
	for i := 0; i < 10; i++ {
		r.Resolve(context.Background(), tenantID, uuid.New(), Recipient{})
	}
	if n := len(r.entries); n > 3 {
		t.Fatalf("cache holds %d entries, want at most 3", n)
	}
}
