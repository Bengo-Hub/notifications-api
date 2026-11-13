package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

func decodeEscrow(t *testing.T, raw string) escrowEvent {
	t.Helper()
	var e escrowEvent
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// treasury escrow/reconcile.go ReconcileAll, a tenant on its own PayHero account.
const escrowOwnShortfall = `{"event_type":"escrow.reconciliation_shortfall","tenant_id":"6f1c0a2e-0000-4000-8000-000000000001",
	"payload":{"wallet_balance":"1500.00","pots_total":"2000.00","in_flight":"300.00","currency":"KES",
	"as_of":"2026-11-13T07:00:00Z","shortfall":"800.00","incident_id":"0b7d4c1e-1111-4000-8000-000000000009"}}`

// The same pass on the shared root account (vendor 12663): one event per tenant on it.
const escrowSharedShortfallA = `{"tenant_id":"6f1c0a2e-0000-4000-8000-0000000000a1",
	"payload":{"wallet_balance":"5000.00","pots_total":"4000.00","in_flight":"0.00","currency":"KES","as_of":"2026-11-13T07:00:00Z",
	"scope":"shared_account","vendor_id":12663,"shared_owed":"6200.00","shortfall":"1200.00","incident_id":"inc-a"}}`

const escrowSharedShortfallB = `{"tenant_id":"6f1c0a2e-0000-4000-8000-0000000000b2",
	"payload":{"wallet_balance":"5000.00","pots_total":"2200.00","in_flight":"0.00","currency":"KES","as_of":"2026-11-13T07:00:00Z",
	"scope":"shared_account","vendor_id":12663,"shared_owed":"6200.00","shortfall":"1200.00","incident_id":"inc-b"}}`

func TestEscrowShortfallOwnAccount(t *testing.T) {
	e := decodeEscrow(t, escrowOwnShortfall)
	if e.Payload.shared() {
		t.Fatal("no scope means the tenant's own account")
	}
	tenants := []escrowTenant{{ID: e.TenantID, Name: "Small Steps", Pots: e.Payload.PotsTotal, InFlight: e.Payload.InFlight}}
	a := escrowOpsAlert(false, e.Payload, tenants)
	if a.Template != escrowShortfallTemplate || !a.WhatsApp || a.TenantID != e.TenantID {
		t.Fatalf("shortfall alert %+v: want the shortfall template, WhatsApp on", a)
	}
	if a.Subject != "Escrow shortfall: Small Steps wallet is KES 800 short" {
		t.Fatalf("subject %q", a.Subject)
	}
	if strings.Contains(a.Text, "\n") || !strings.Contains(a.Text, "Small Steps (own PayHero account)") ||
		!strings.Contains(a.Text, "KES 1,500 against KES 2,300 owed, KES 800 short") || !strings.Contains(a.Text, "recovers by itself") {
		t.Fatalf("text %q", a.Text)
	}
	html := renderEmail(t, escrowShortfallTemplate, a.Data)
	for _, want := range []string{"Escrow shortfall for Small Steps", "Own account", "KES 800", "KES 1,500", "KES 2,300",
		"This is the tenant's own wallet", "Team wallet withdrawal and the daily commission sweep", "top up the wallet",
		"recovers by itself", "13 Nov 2026, 10:00 EAT", "0b7d4c1e-1111-4000-8000-000000000009",
		"Small Steps: pots KES 2,000, in flight KES 300"} {
		if !strings.Contains(html, want) {
			t.Errorf("own shortfall lacks %q:\n%s", want, html)
		}
	}
	for _, absent := range []string{"Shared account", "vendor"} {
		if strings.Contains(html, absent) {
			t.Errorf("own shortfall shows %q", absent)
		}
	}
	noBlankLabels(t, "own shortfall", html)

	// The recovery: email and Slack only.
	r := escrowOpsAlert(true, e.Payload, tenants)
	if r.Template != escrowRecoveredTemplate || r.WhatsApp || r.Subject != "Escrow recovered: Small Steps wallet" {
		t.Fatalf("recovery alert %+v", r)
	}
	html = renderEmail(t, escrowRecoveredTemplate, r.Data)
	for _, want := range []string{"Escrow recovered for Small Steps", "Own account", "this tenant. Nothing else needs doing"} {
		if !strings.Contains(html, want) {
			t.Errorf("own recovery lacks %q:\n%s", want, html)
		}
	}
	noBlankLabels(t, "own recovery", html)
}

func TestEscrowShortfallSharedAccount(t *testing.T) {
	a1, b1 := decodeEscrow(t, escrowSharedShortfallA), decodeEscrow(t, escrowSharedShortfallB)
	if !a1.Payload.shared() || a1.Payload.VendorID != 12663 {
		t.Fatalf("shared payload %+v", a1.Payload)
	}
	tenants := []escrowTenant{
		{ID: a1.TenantID, Name: "Hadia", Pots: a1.Payload.PotsTotal, InFlight: a1.Payload.InFlight},
		{ID: b1.TenantID, Name: "codevertex", Pots: b1.Payload.PotsTotal, InFlight: b1.Payload.InFlight},
	}
	a := escrowOpsAlert(false, a1.Payload, tenants)
	if !a.WhatsApp || a.Subject != "Escrow shortfall: shared PayHero account 12663 is KES 1,200 short" {
		t.Fatalf("shared alert %+v", a)
	}
	if !strings.Contains(a.Text, "shared PayHero account 12663 (Hadia, codevertex)") || !strings.Contains(a.Text, "KES 5,000 against KES 6,200 owed") {
		t.Fatalf("text %q", a.Text)
	}
	html := renderEmail(t, escrowShortfallTemplate, a.Data)
	for _, want := range []string{"Escrow shortfall on shared PayHero account 12663", "Shared account, PayHero vendor 12663",
		"shared by 2 tenants", "Owed by every tenant on the account", "KES 6,200", "KES 1,200", "each tenant on this account",
		"Tenants", "<li>Hadia: pots KES 4,000</li>", "<li>codevertex: pots KES 2,200</li>"} {
		if !strings.Contains(html, want) {
			t.Errorf("shared shortfall lacks %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, "in flight KES") {
		t.Error("a zero in flight must not render")
	}
	noBlankLabels(t, "shared shortfall", html)

	r := escrowOpsAlert(true, a1.Payload, tenants)
	html = renderEmail(t, escrowRecoveredTemplate, r.Data)
	for _, want := range []string{"Escrow recovered on shared PayHero account 12663", "Hadia, codevertex", "KES 5,000 against KES 6,200 owed"} {
		if !strings.Contains(html, want) {
			t.Errorf("shared recovery lacks %q:\n%s", want, html)
		}
	}
	noBlankLabels(t, "shared recovery", html)

	// No time and no incident id: those lines are left out, never blank.
	bare := escrowPayload{WalletBalance: "10.00", PotsTotal: "20.00", Shortfall: "10.00"}
	html = renderEmail(t, escrowShortfallTemplate, escrowAlertData(false, bare, []escrowTenant{{ID: "t-1"}}))
	if strings.Contains(html, "Checked") || strings.Contains(html, "Incident") {
		t.Errorf("bare payload shows an optional line:\n%s", html)
	}
	noBlankLabels(t, "bare shortfall", html)
}

// The subscriber: the subjects map to the handlers, siblings on a shared account become one alert,
// and an incident alerts once per state.
func TestEscrowSubscriberMapping(t *testing.T) {
	s := &Subscriber{log: zap.NewNop()}
	saved := escrowSharedWindow
	escrowSharedWindow = time.Hour // the test flushes by hand
	defer func() { escrowSharedWindow = saved }()

	s.handleEscrowShortfall(&nats.Msg{Data: []byte(escrowSharedShortfallA)})
	s.handleEscrowShortfall(&nats.Msg{Data: []byte(escrowSharedShortfallB)})
	s.handleEscrowShortfall(&nats.Msg{Data: []byte(escrowSharedShortfallA)}) // a redelivery
	b := s.escrow.batches["12663:short"]
	if len(s.escrow.batches) != 1 || b == nil || len(b.tenants) != 2 || len(b.msgs) != 3 || b.recovered {
		t.Fatalf("batches %+v", s.escrow.batches)
	}
	s.flushEscrowBatch("12663:short")
	if len(s.escrow.batches) != 0 || s.escrow.mem[escrowAlertKeyPrefix+"shared:12663"].state != "short" {
		t.Fatalf("after flush: batches %v, state %+v", s.escrow.batches, s.escrow.mem)
	}

	// A late sibling, or the whole batch redelivered, does not alert again.
	ctx := context.Background()
	key := escrowAlertKey(decodeEscrow(t, escrowSharedShortfallB).Payload, "", time.Now())
	if s.escrow.transition(ctx, key, "short") {
		t.Error("a second shortfall for the same shared account must not alert")
	}
	if !s.escrow.transition(ctx, key, "recovered") || s.escrow.transition(ctx, key, "recovered") {
		t.Error("the recovery alerts once")
	}
	if !s.escrow.transition(ctx, key, "short") {
		t.Error("a shortfall after a recovery is a new incident and alerts")
	}

	// An own-account event is sent at once, keyed on its incident.
	s.handleEscrowRecovered(&nats.Msg{Data: []byte(escrowOwnShortfall)})
	if len(s.escrow.batches) != 0 || s.escrow.mem[escrowAlertKeyPrefix+"incident:0b7d4c1e-1111-4000-8000-000000000009"].state != "recovered" {
		t.Fatalf("own event: batches %v, state %+v", s.escrow.batches, s.escrow.mem)
	}
	if got := escrowAlertKey(escrowPayload{}, "t-1", time.Date(2026, 11, 13, 22, 0, 0, 0, time.UTC)); got != escrowAlertKeyPrefix+"tenant:t-1:2026-11-13" {
		t.Errorf("fallback key %q", got)
	}

	// A malformed event is acked and dropped, with nothing gathered.
	s.handleEscrowShortfall(&nats.Msg{Data: []byte("not json")})
	if len(s.escrow.batches) != 0 {
		t.Error("a malformed event must not be gathered")
	}
}
