package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// memDigestStore is digestStore in memory, with the same once-a-day and lease rules as Postgres.
type memDigestStore struct {
	mu    sync.Mutex
	items []*memItem
	sends map[string]*memSend
}

type memItem struct {
	digestItem
	sendID *uuid.UUID
}

type memSend struct {
	id        uuid.UUID
	kind      string
	tenant    uuid.UUID
	recipient string
	date      string
	status    string
	channels  []string
	claimedAt time.Time
	sentAt    time.Time
	count     int
}

func newMemDigestStore() *memDigestStore { return &memDigestStore{sends: map[string]*memSend{}} }

func sendKey(kind string, tenantID uuid.UUID, recipient, date string) string {
	return kind + "|" + tenantID.String() + "|" + recipient + "|" + date
}

func (s *memDigestStore) Add(_ context.Context, it digestItem) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.items {
		if x.TenantID == it.TenantID && x.Kind == it.Kind && x.EventID == it.EventID {
			return false, nil
		}
	}
	s.items = append(s.items, &memItem{digestItem: it})
	return true, nil
}

func (s *memDigestStore) Tenants(_ context.Context, kind string) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, x := range s.items {
		if x.Kind == kind && x.sendID == nil && !seen[x.TenantID] {
			seen[x.TenantID] = true
			out = append(out, x.TenantID)
		}
	}
	return out, nil
}

func (s *memDigestStore) Recipients(_ context.Context, kind string, tenantID uuid.UUID, upTo, after string, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, x := range s.items {
		if x.Kind == kind && x.TenantID == tenantID && x.sendID == nil && x.LocalDate <= upTo && x.RecipientKey > after && !seen[x.RecipientKey] {
			seen[x.RecipientKey] = true
			out = append(out, x.RecipientKey)
		}
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memDigestStore) Claim(_ context.Context, kind string, tenantID uuid.UUID, recipient, date string, now, staleBefore time.Time) (digestClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := sendKey(kind, tenantID, recipient, date)
	cur, ok := s.sends[k]
	if !ok {
		cur = &memSend{id: uuid.New(), kind: kind, tenant: tenantID, recipient: recipient, date: date, status: "sending", claimedAt: now}
		s.sends[k] = cur
		return digestClaim{SendID: cur.id}, true, nil
	}
	if cur.status != "sending" || !cur.claimedAt.Before(staleBefore) {
		return digestClaim{}, false, nil
	}
	cur.claimedAt = now
	return digestClaim{SendID: cur.id, Channels: append([]string(nil), cur.channels...)}, true, nil
}

func (s *memDigestStore) Stalled(_ context.Context, kind string, staleBefore time.Time, limit int) ([]digestStalled, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []digestStalled
	for _, v := range s.sends {
		if v.kind == kind && v.status == "sending" && v.claimedAt.Before(staleBefore) && len(out) < limit {
			out = append(out, digestStalled{TenantID: v.tenant, RecipientKey: v.recipient, LocalDate: v.date})
		}
	}
	return out, nil
}

func (s *memDigestStore) Items(_ context.Context, sendID uuid.UUID, kind string, tenantID uuid.UUID, recipient, upTo string, attach bool) ([]digestItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []digestItem
	for _, x := range s.items {
		if attach && x.sendID == nil && x.Kind == kind && x.TenantID == tenantID && x.RecipientKey == recipient && x.LocalDate <= upTo {
			id := sendID
			x.sendID = &id
		}
		if x.sendID != nil && *x.sendID == sendID {
			out = append(out, x.digestItem)
		}
	}
	return out, nil
}

func (s *memDigestStore) send(id uuid.UUID) *memSend {
	for _, v := range s.sends {
		if v.id == id {
			return v
		}
	}
	return nil
}

func (s *memDigestStore) MarkChannel(_ context.Context, sendID uuid.UUID, channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.send(sendID); v != nil && !digestHas(v.channels, channel) {
		v.channels = append(v.channels, channel)
	}
	return nil
}

func (s *memDigestStore) Finish(_ context.Context, sendID uuid.UUID, items int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v := s.send(sendID); v != nil {
		v.status, v.count, v.sentAt = "sent", items, now
	}
	return nil
}

func (s *memDigestStore) Purge(context.Context, time.Time) error { return nil }

// sentNotice is one notice the job queued, one channel each.
type sentNotice struct {
	channel string
	notice  partyNotice
}

type digestHarness struct {
	store  *memDigestStore
	tenant uuid.UUID
	now    time.Time
	sent   []sentNotice
	fail   func(channel string) error
	opted  map[string]bool
	mu     sync.Mutex
}

func newDigestHarness(t *testing.T, now time.Time) *digestHarness {
	t.Helper()
	return &digestHarness{store: newMemDigestStore(), tenant: uuid.New(), now: now, opted: map[string]bool{}}
}

// job is one pod's digest job over the shared store.
func (h *digestHarness) job() *hadiaDigest {
	return &hadiaDigest{
		store:   h.store,
		resolve: func(context.Context, string) (*tenantInfo, error) { return &tenantInfo{Country: "KE"}, nil },
		publish: func(_ context.Context, n partyNotice) error {
			ch := n.Parties[0].Channels[0]
			if h.fail != nil {
				if err := h.fail(ch); err != nil {
					return err
				}
			}
			h.mu.Lock()
			h.sent = append(h.sent, sentNotice{channel: ch, notice: n})
			h.mu.Unlock()
			return nil
		},
		suppressed: func(_ context.Context, _ uuid.UUID, ch, addr string) (bool, error) { return h.opted[ch+":"+addr], nil },
		now:        func() time.Time { return h.now },
		log:        zap.NewNop(),
	}
}

func (h *digestHarness) gift(t *testing.T, owner, registry, title, amount string, at time.Time) eventslib.Event {
	t.Helper()
	evt := eventslib.Event{ID: uuid.New(), EventType: "gift.received", TenantID: h.tenant, Timestamp: at, Payload: map[string]any{
		"owner_id": owner, "owner_name": "Amina", "owner_email": "amina@example.com", "owner_phone": "+254712345678",
		"owner_digest": "daily", "registry_id": registry, "registry_title": title, "amount": amount, "currency": "KES",
		"guest_name": "Wanjiru Kamau", "message": "Karibu sana", "received_at": at.UTC().Format(time.RFC3339),
		"owner_url": "https://hadiafrica.com/app/registries/" + registry + "/gifts"}}
	if err := h.job().hold(context.Background(), &tenantInfo{}, evt); err != nil {
		t.Fatal(err)
	}
	return evt
}

func eat(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, digestTZ)
}

func (h *digestHarness) channels() []string {
	var out []string
	for _, s := range h.sent {
		out = append(out, s.channel)
	}
	sort.Strings(out)
	return out
}

// A daily owner's gift alert is held, never sent; an instant owner's goes out as before; the
// guest receipt is never held.
func TestDigestHoldsOnlyDailyOwnerAlerts(t *testing.T) {
	d := &hadiaDigest{}
	receipt, alert := hadiaMappings["gift.received"][0], hadiaMappings["gift.received"][1]
	if !alert.Digest || receipt.Digest {
		t.Fatal("only the owner alert may be held")
	}
	if !heldForDigest(alert, map[string]any{"owner_digest": "daily"}, d) {
		t.Error("a daily owner's alert must be held")
	}
	for _, v := range []string{"instant", "", "weekly"} {
		if heldForDigest(alert, map[string]any{"owner_digest": v}, d) {
			t.Errorf("owner_digest %q must send at once", v)
		}
	}
	if heldForDigest(receipt, map[string]any{"owner_digest": "daily"}, d) {
		t.Error("the guest receipt must never be held")
	}
	if heldForDigest(alert, map[string]any{"owner_digest": "daily"}, nil) {
		t.Error("with no digest store every alert goes at once")
	}
	for event, msgs := range hadiaMappings {
		for i, m := range msgs {
			if m.Digest && !(event == "gift.received" && i == 1) {
				t.Errorf("%s[%d]: only the gift owner alert is held; refunds, payouts and freezes never are", event, i)
			}
		}
	}
}

// One summary per owner per day: three gifts over two registries add up, the button opens the
// dashboard, guest names and messages stay out, and the idempotency key is digest:tenant:owner:date.
func TestDigestAggregates(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 18, 2))
	owner, r1, r2 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	h.gift(t, owner, r1, "Amina and Tom's wedding", "5000.00", eat(2026, 11, 5, 9, 0))
	h.gift(t, owner, r1, "Amina and Tom's wedding", "2500.50", eat(2026, 11, 5, 12, 0))
	h.gift(t, owner, r2, "Baby shower", "1000.00", eat(2026, 11, 5, 17, 30))
	h.job().Run(context.Background())
	if got := h.channels(); strings.Join(got, ",") != "email,whatsapp" {
		t.Fatalf("channels %v, want one email and one WhatsApp", got)
	}
	n := h.sent[0].notice
	if n.Data["gifts"] != "3 gifts" || n.Data["total"] != "KES 8,500.50" || n.Data["where"] != "2 registries" || n.Data["day"] != "5 Nov 2026" {
		t.Fatalf("data %v", n.Data)
	}
	if n.Data["action_link"] != "https://hadiafrica.com/app" || n.Parties[0].WhatsApp.Button != "app" {
		t.Errorf("several registries open the dashboard: %v %q", n.Data["action_link"], n.Parties[0].WhatsApp.Button)
	}
	if !strings.HasPrefix(n.IdemBase, "digest:"+h.tenant.String()+":") || !strings.HasSuffix(n.IdemBase, ":2026-11-05") {
		t.Errorf("idempotency key %q", n.IdemBase)
	}
	if params := n.Parties[0].WhatsApp.Params; len(params) != 5 || params[0] != "Amina" || params[2] != "3 gifts" {
		t.Errorf("params %v", params)
	}
	raw, _ := json.Marshal(n.Data)
	if strings.Contains(string(raw), "Wanjiru") || strings.Contains(string(raw), "Karibu") {
		t.Errorf("a guest name or message reached the summary: %s", raw)
	}
	regs, _ := n.Data["registries"].([]map[string]any)
	if len(regs) != 2 || regs[0]["title"] != "Amina and Tom's wedding" || regs[0]["gifts"] != "2 gifts" || regs[0]["total"] != "KES 7,500.50" {
		t.Errorf("lines %v", regs)
	}
}

// One registry: the button opens its gift log.
func TestDigestOneRegistryLinksItsGiftLog(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 18, 0))
	reg := uuid.NewString()
	h.gift(t, uuid.NewString(), reg, "Wedding", "100.00", eat(2026, 11, 5, 10, 0))
	h.job().Run(context.Background())
	if len(h.sent) != 2 {
		t.Fatalf("sent %d", len(h.sent))
	}
	n := h.sent[0].notice
	if n.Parties[0].WhatsApp.Button != "app/registries/"+reg+"/gifts" || n.Data["where"] != "Wedding" || n.Data["gifts"] != "1 gift" {
		t.Errorf("button %q data %v", n.Parties[0].WhatsApp.Button, n.Data)
	}
}

// A redelivered event is held once.
func TestDigestHoldIsIdempotent(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 18, 0))
	evt := h.gift(t, uuid.NewString(), uuid.NewString(), "Wedding", "100.00", eat(2026, 11, 5, 10, 0))
	if err := h.job().hold(context.Background(), &tenantInfo{}, evt); err != nil {
		t.Fatal(err)
	}
	if len(h.store.items) != 1 {
		t.Fatalf("items %d, want 1", len(h.store.items))
	}
	h.job().Run(context.Background())
	if n := h.sent[0].notice; n.Data["gifts"] != "1 gift" {
		t.Errorf("gifts %v", n.Data["gifts"])
	}
}

// 17:59 belongs to today's summary, 18:00 to tomorrow's; nothing goes before 18:00.
func TestDigestTimezoneBoundary(t *testing.T) {
	if got := digestDate(eat(2026, 11, 5, 17, 59), 18); got != "2026-11-05" {
		t.Errorf("17:59 EAT in %s", got)
	}
	if got := digestDate(eat(2026, 11, 5, 18, 0), 18); got != "2026-11-06" {
		t.Errorf("18:00 EAT in %s", got)
	}
	// 15:30 UTC is 18:30 in Nairobi: the next day's summary.
	if got := digestDate(time.Date(2026, 11, 5, 15, 30, 0, 0, time.UTC), 18); got != "2026-11-06" {
		t.Errorf("15:30 UTC in %s", got)
	}
	if got := digestDue(eat(2026, 11, 5, 17, 59), 18); got != "2026-11-04" {
		t.Errorf("due at 17:59: %s", got)
	}
	if got := digestDue(eat(2026, 11, 5, 18, 0), 18); got != "2026-11-05" {
		t.Errorf("due at 18:00: %s", got)
	}

	h := newDigestHarness(t, eat(2026, 11, 5, 17, 59))
	owner, reg := uuid.NewString(), uuid.NewString()
	h.gift(t, owner, reg, "Wedding", "100.00", eat(2026, 11, 5, 17, 59))
	h.gift(t, owner, reg, "Wedding", "200.00", eat(2026, 11, 5, 18, 0))
	h.job().Run(context.Background())
	if len(h.sent) != 0 {
		t.Fatalf("sent before 18:00: %d", len(h.sent))
	}
	h.now = eat(2026, 11, 5, 18, 5)
	h.job().Run(context.Background())
	if len(h.sent) != 2 || h.sent[0].notice.Data["total"] != "KES 100" {
		t.Fatalf("the 18:00 run must send only the 17:59 gift: %d %v", len(h.sent), h.sent)
	}
	h.now = eat(2026, 11, 6, 18, 0)
	h.job().Run(context.Background())
	if len(h.sent) != 4 || h.sent[2].notice.Data["total"] != "KES 200" {
		t.Fatalf("the next day sends the 18:00 gift: %d", len(h.sent))
	}
}

// A day with no gifts sends nothing, and a second run of a sent day sends nothing again.
func TestDigestEmptyDaySendsNothing(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 19, 0))
	h.job().Run(context.Background())
	if len(h.sent) != 0 {
		t.Fatalf("empty day sent %d", len(h.sent))
	}
	h.gift(t, uuid.NewString(), uuid.NewString(), "Wedding", "100.00", eat(2026, 11, 5, 10, 0))
	h.job().Run(context.Background())
	h.job().Run(context.Background())
	if len(h.sent) != 2 {
		t.Fatalf("sent %d, want 2", len(h.sent))
	}
}

// Two pods running at the same moment send one summary.
func TestDigestTwoPodsSendOnce(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 18, 0))
	h.gift(t, uuid.NewString(), uuid.NewString(), "Wedding", "100.00", eat(2026, 11, 5, 10, 0))
	a, b := h.job(), h.job()
	var wg sync.WaitGroup
	for _, j := range []*hadiaDigest{a, b} {
		wg.Add(1)
		go func(j *hadiaDigest) {
			defer wg.Done()
			j.Run(context.Background())
		}(j)
	}
	wg.Wait()
	if got := h.channels(); strings.Join(got, ",") != "email,whatsapp" {
		t.Fatalf("two pods sent %v", got)
	}
}

// A pod that dies after the email: the claim waits out its lease, then the next run sends only the
// WhatsApp, with the same items, and never repeats the email.
func TestDigestResumesAfterCrash(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 18, 0))
	owner, reg := uuid.NewString(), uuid.NewString()
	h.gift(t, owner, reg, "Wedding", "100.00", eat(2026, 11, 5, 10, 0))
	h.fail = func(ch string) error {
		if ch == "whatsapp" {
			return errors.New("bus down")
		}
		return nil
	}
	h.job().Run(context.Background())
	if got := h.channels(); strings.Join(got, ",") != "email" {
		t.Fatalf("first attempt sent %v", got)
	}
	h.fail = nil
	// A gift after the claim waits for tomorrow rather than changing a half-sent summary.
	h.gift(t, owner, reg, "Wedding", "900.00", eat(2026, 11, 5, 17, 0))
	h.now = eat(2026, 11, 5, 18, 5)
	h.job().Run(context.Background())
	if len(h.sent) != 1 {
		t.Fatalf("resumed inside the lease: %v", h.channels())
	}
	h.now = eat(2026, 11, 5, 18, 0).Add(digestLease + time.Minute)
	h.job().Run(context.Background())
	if got := h.channels(); strings.Join(got, ",") != "email,whatsapp" {
		t.Fatalf("after the lease: %v", got)
	}
	if h.sent[1].notice.Data["total"] != "KES 100" {
		t.Errorf("the resumed channel must carry the same items: %v", h.sent[1].notice.Data["total"])
	}
	h.now = eat(2026, 11, 6, 18, 0)
	h.job().Run(context.Background())
	if len(h.sent) != 4 || h.sent[2].notice.Data["total"] != "KES 900" {
		t.Fatalf("the late gift goes in the next day's summary: %d", len(h.sent))
	}
}

// A half-sent summary is finished even when no gift is waiting any more: its items are already
// taken, so only the stalled claim brings it back.
func TestDigestResumesWithNothingWaiting(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 18, 0))
	h.gift(t, uuid.NewString(), uuid.NewString(), "Wedding", "100.00", eat(2026, 11, 5, 10, 0))
	h.fail = func(ch string) error {
		if ch == "whatsapp" {
			return errors.New("bus down")
		}
		return nil
	}
	h.job().Run(context.Background())
	h.fail = nil
	h.now = h.now.Add(digestLease + time.Minute)
	h.job().Run(context.Background())
	if got := h.channels(); strings.Join(got, ",") != "email,whatsapp" {
		t.Fatalf("sent %v", got)
	}
	for _, v := range h.store.sends {
		if v.status != "sent" || v.count != 1 {
			t.Errorf("marker %+v", v)
		}
	}
}

// An address on the suppression list gets nothing on that channel; the other channel still goes.
func TestDigestHonoursOptOut(t *testing.T) {
	h := newDigestHarness(t, eat(2026, 11, 5, 18, 0))
	h.opted["whatsapp:+254712345678"] = true
	h.gift(t, uuid.NewString(), uuid.NewString(), "Wedding", "100.00", eat(2026, 11, 5, 10, 0))
	h.job().Run(context.Background())
	if got := h.channels(); strings.Join(got, ",") != "email" {
		t.Fatalf("sent %v, want email only", got)
	}
	h.now = h.now.Add(2 * digestLease)
	h.job().Run(context.Background())
	if len(h.sent) != 1 {
		t.Fatal("an opted-out channel is done, not retried")
	}
}

// The digest templates exist for both channels and the WhatsApp template is declared for Meta.
func TestDigestTemplates(t *testing.T) {
	for _, p := range []string{"email/" + hadiaDigestTemplate + ".html", "whatsapp/" + hadiaDigestTemplate + ".txt"} {
		if _, err := os.Stat(filepath.Join("..", "..", "templates", filepath.FromSlash(p))); err != nil {
			t.Errorf("%s missing", p)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "whatsapp", "templatesync", "templates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"`+hadiaDigestWATemplate+`"`) {
		t.Errorf("%s is not declared in templates.json", hadiaDigestWATemplate)
	}
	if digestHourOf(&tenantInfo{DigestHour: 20}) != 20 || digestHourOf(&tenantInfo{}) != 18 || digestHourOf(nil) != 18 {
		t.Error("digest hour")
	}
	if metadataHour(float64(19)) != 19 || metadataHour("7") != 7 || metadataHour(float64(24)) != 0 || metadataHour(true) != 0 {
		t.Error("metadataHour")
	}
}
