package wacost

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/credits"
)

const (
	platID = "11111111-1111-1111-1111-111111111111"
	ulID   = "22222222-2222-2222-2222-222222222222"
	badID  = "33333333-3333-3333-3333-333333333333"
)

type fakeAccounts []Account

func (f fakeAccounts) WhatsAppCostAccounts(context.Context) ([]Account, error) { return f, nil }

type memCache struct {
	mu   sync.Mutex
	vals map[string][]byte
	ttls map[string]time.Duration
}

func newMemCache() *memCache {
	return &memCache{vals: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (c *memCache) Get(_ context.Context, k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vals[k]
	return v, ok
}

func (c *memCache) Set(_ context.Context, k string, v []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vals[k], c.ttls[k] = v, ttl
}

// fakeGraph serves a shared platform WABA with the platform's number and Urban Loft's number on
// it, and a tenant WABA that Meta refuses.
func fakeGraph(t *testing.T, calls *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v26.0/WABA_P":
			if r.Header.Get("Authorization") != "Bearer platform-token" {
				t.Errorf("shared WABA read without the platform token: %q", r.Header.Get("Authorization"))
			}
			if r.URL.Query().Get("access_token") != "" {
				t.Error("the token must travel in a header, never the URL")
			}
			f := r.URL.Query().Get("fields")
			if !strings.Contains(f, "pricing_analytics.start(") || !strings.Contains(f, "dimensions(PHONE,PRICING_CATEGORY)") || !strings.Contains(f, "currency") {
				t.Errorf("fields %q", f)
			}
			_, _ = w.Write([]byte(`{"id":"WABA_P","currency":"USD","pricing_analytics":{"data":[{"data_points":[
				{"start":1,"end":2,"phone_number":"254700000001","pricing_category":"UTILITY","pricing_type":"REGULAR","volume":10,"cost":0.5},
				{"start":1,"end":2,"phone_number":"254700000002","pricing_category":"MARKETING","pricing_type":"REGULAR","volume":20,"cost":2.0},
				{"start":2,"end":3,"phone_number":"254700000002","pricing_category":"MARKETING_LITE","pricing_type":"REGULAR","volume":4,"cost":0.25},
				{"start":2,"end":3,"phone_number":"254700000002","pricing_category":"SERVICE","pricing_type":"FREE_CUSTOMER_SERVICE","volume":5,"cost":0}
			]}]}}`))
		case "/v26.0/WABA_P/phone_numbers":
			_, _ = w.Write([]byte(`{"data":[
				{"id":"PN1","display_phone_number":"+254 700 000001","verified_name":"Codevertex"},
				{"id":"PN2","display_phone_number":"+254 700 000002","verified_name":"Urban Loft"}]}`))
		case "/v26.0/WABA_N":
			// Meta answered without splitting by number.
			_, _ = w.Write([]byte(`{"id":"WABA_N","currency":"USD","pricing_analytics":{"data":[{"data_points":[
				{"start":1,"end":2,"pricing_category":"UTILITY","volume":7,"cost":0.75},
				{"start":2,"end":3,"pricing_category":"MARKETING","volume":3,"cost":0.25}
			]}]}}`))
		case "/v26.0/WABA_N/phone_numbers":
			_, _ = w.Write([]byte(`{"data":[{"id":"PN9","display_phone_number":"+254 700 000009","verified_name":"Solo"}]}`))
		case "/v26.0/WABA_BAD":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100}}`))
		default:
			t.Errorf("unexpected Graph call %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func fakeSubscriptions(t *testing.T) *credits.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/s2s/credits/whatsapp-usage" || r.Header.Get("X-API-Key") != "svc-key" {
			t.Errorf("unexpected subscriptions call %s", r.URL.Path)
		}
		if r.URL.Query().Get("from") != "2026-11-01" || r.URL.Query().Get("to") != "2026-11-12" {
			t.Errorf("range %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"from":"2026-11-01","to":"2026-11-12","currency":"KES","tenants":[
			{"tenant_id":"` + ulID + `","slug":"urban-loft","name":"Urban Loft","categories":[
				{"service":"whatsapp_marketing","units_bought":100,"kes_paid":150,"units_consumed":24,"unit_price_kes":1.5,"consumed_value_kes":36}]},
			{"tenant_id":"` + badID + `","slug":"bad-co","name":"Bad Co","categories":[
				{"service":"whatsapp_utility","units_bought":0,"kes_paid":0,"units_consumed":5,"unit_price_kes":0.8,"consumed_value_kes":4}]}
		]}`))
	}))
	t.Cleanup(srv.Close)
	cfg := serviceclient.DefaultConfig(srv.URL, "subscriptions-api", zap.NewNop())
	cfg.MaxElapsedTime = 300 * time.Millisecond
	return credits.New(serviceclient.New(cfg), "svc-key")
}

func testService(t *testing.T, calls *int64, cache Cache) *Service {
	t.Helper()
	g := fakeGraph(t, calls)
	t.Cleanup(g.Close)
	accounts := fakeAccounts{
		{TenantID: platID, IsPlatform: true, WABAID: "WABA_P", PhoneNumberID: "PN1", AccessToken: "platform-token", TokenSource: "platform", APIVersion: "v26.0"},
		{TenantID: ulID, WABAID: "WABA_P", PhoneNumberID: "PN2", AccessToken: "platform-token", TokenSource: "platform", APIVersion: "v26.0"},
		{TenantID: badID, WABAID: "WABA_BAD", PhoneNumberID: "PN3", AccessToken: "own-token", TokenSource: "own", APIVersion: "v26.0"},
	}
	svc := NewService(accounts, fakeSubscriptions(t), &Graph{BaseURL: g.URL}, cache, nil, zap.NewNop())
	svc.MetaTimeout = 2 * time.Second
	svc.ReportTimeout = 5 * time.Second
	return svc
}

func testRange(t *testing.T) Range {
	t.Helper()
	rng, err := ParseRange("2026-11-01", "2026-11-12", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rng
}

func findRow(t *testing.T, w WABA, phoneID string) Row {
	t.Helper()
	for _, r := range w.Rows {
		if r.PhoneNumberID == phoneID {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", phoneID, w.Rows)
	return Row{}
}

func TestSharedWABASplitsPerNumber(t *testing.T) {
	var calls int64
	rep, err := testService(t, &calls, nil).Build(context.Background(), testRange(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.WABAs) != 2 {
		t.Fatalf("want 2 WABAs, got %d", len(rep.WABAs))
	}
	shared := rep.WABAs[0]
	if shared.WABAID != "WABA_P" || shared.Owner != "platform" || shared.TokenSource != "platform" || shared.Error != "" {
		t.Fatalf("shared WABA %+v", shared)
	}
	plat := findRow(t, shared, "PN1")
	if plat.Tenant != "platform" || plat.MetaMessages != 10 || plat.MetaCost != 0.5 || plat.Meta[CategoryUtility].Messages != 10 {
		t.Fatalf("platform row %+v", plat)
	}
	if plat.CreditsUsed != 0 {
		t.Fatalf("platform sends are never billed, got %d credits", plat.CreditsUsed)
	}
	ul := findRow(t, shared, "PN2")
	if ul.Tenant != "urban-loft" || ul.TenantID != ulID {
		t.Fatalf("Urban Loft row owner %+v", ul)
	}
	// MARKETING_LITE folds into marketing.
	if ul.Meta[CategoryMarketing].Messages != 24 || ul.Meta[CategoryMarketing].Cost != 2.25 || ul.Meta[CategoryService].Messages != 5 {
		t.Fatalf("Urban Loft Meta usage %+v", ul.Meta)
	}
	if ul.MetaCurrency != "USD" || ul.CreditsUsed != 24 || ul.KESPaid != 150 || ul.ConsumedValue != 36 {
		t.Fatalf("Urban Loft credits %+v", ul)
	}
	if ul.MarginKES != nil || ul.MetaCostKES != nil || !shared.RateUnavailable || !rep.Totals.RateUnavailable {
		t.Fatal("without a rate the USD cost is not converted and no margin is shown")
	}
	if rep.Totals.MetaCost["USD"] != 2.75 || rep.Totals.MetaMessages != 39 {
		t.Fatalf("totals %+v", rep.Totals)
	}
}

func TestRefusedWABADoesNotBreakReport(t *testing.T) {
	var calls int64
	rep, err := testService(t, &calls, nil).Build(context.Background(), testRange(t))
	if err != nil {
		t.Fatal(err)
	}
	bad := rep.WABAs[1]
	if bad.WABAID != "WABA_BAD" || !strings.Contains(bad.Error, "Unsupported get request") {
		t.Fatalf("refused WABA should carry Meta's words, got %+v", bad)
	}
	row := findRow(t, bad, "PN3")
	// The tenant's own credits still show although Meta's side is missing.
	if row.TenantID != badID || row.CreditsUsed != 5 || row.MetaCost != 0 {
		t.Fatalf("refused WABA row %+v", row)
	}
	if rep.WABAs[0].Error != "" || len(rep.WABAs[0].Rows) != 2 {
		t.Fatal("the healthy WABA must still be complete")
	}
	if rep.Totals.MarginKES != nil {
		t.Fatal("no total margin when a WABA is missing")
	}
}

func TestReportIsCachedPerRange(t *testing.T) {
	var calls int64
	cache := newMemCache()
	svc := testService(t, &calls, cache)
	rng := testRange(t)
	first, err := svc.Build(context.Background(), rng)
	if err != nil || first.Cached {
		t.Fatalf("first build cached=%v err=%v", first.Cached, err)
	}
	after := atomic.LoadInt64(&calls)
	second, err := svc.Build(context.Background(), rng)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached || atomic.LoadInt64(&calls) != after {
		t.Fatalf("second build should come from the cache (cached=%v, Graph calls %d then %d)", second.Cached, after, calls)
	}
	if len(second.WABAs) != 2 || second.WABAs[0].Rows[0].MetaMessages == 0 && second.WABAs[0].Rows[1].MetaMessages == 0 {
		t.Fatal("cached report lost its rows")
	}
	// A WABA failed, so the entry lives only briefly.
	if ttl := cache.ttls[CacheKey(rng)]; ttl != PartialCacheTTL {
		t.Fatalf("partial report cached for %v, want %v", ttl, PartialCacheTTL)
	}
}

type fixedRates map[string]float64

func (f fixedRates) KESPer(_ context.Context, cur string) (float64, bool) {
	r, ok := f[cur]
	return r, ok
}

func TestMetaCostConvertedToKESForMargin(t *testing.T) {
	var calls int64
	svc := testService(t, &calls, nil).WithRates(fixedRates{"USD": 130})
	rep, err := svc.Build(context.Background(), testRange(t))
	if err != nil {
		t.Fatal(err)
	}
	shared := rep.WABAs[0]
	if shared.KESPerUnit != 130 || shared.RateUnavailable {
		t.Fatalf("rate on the WABA %+v", shared)
	}
	ul := findRow(t, shared, "PN2")
	if ul.MetaCostKES == nil || *ul.MetaCostKES != 292.5 {
		t.Fatalf("Urban Loft cost in KES %v, want 292.5 (2.25 USD at 130)", ul.MetaCostKES)
	}
	if ul.MarginKES == nil || *ul.MarginKES != 36-292.5 {
		t.Fatalf("Urban Loft margin %v, want %v", ul.MarginKES, 36-292.5)
	}
	// The refused WABA leaves the totals without a KES figure.
	if rep.Totals.MarginKES != nil || rep.Totals.MetaCostKES != nil {
		t.Fatal("no total in KES while a WABA is missing")
	}
}

func TestPricingWithoutPhoneNumbersShowsWholeAccount(t *testing.T) {
	var calls int64
	g := fakeGraph(t, &calls)
	t.Cleanup(g.Close)
	accounts := fakeAccounts{{TenantID: ulID, WABAID: "WABA_N", PhoneNumberID: "PN9", AccessToken: "own-token", TokenSource: "own", APIVersion: "v26.0"}}
	svc := NewService(accounts, fakeSubscriptions(t), &Graph{BaseURL: g.URL}, nil, nil, zap.NewNop()).WithRates(fixedRates{"USD": 100})
	rep, err := svc.Build(context.Background(), testRange(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.WABAs) != 1 || rep.WABAs[0].Error != "" {
		t.Fatalf("report should render, got %+v", rep.WABAs)
	}
	var whole *Row
	for i := range rep.WABAs[0].Rows {
		if rep.WABAs[0].Rows[i].WholeAccount {
			whole = &rep.WABAs[0].Rows[i]
		}
	}
	if whole == nil || whole.MetaMessages != 10 || whole.Meta[CategoryUtility].Messages != 7 {
		t.Fatalf("whole account row %+v", whole)
	}
	if findRow(t, rep.WABAs[0], "PN9").MetaMessages != 0 {
		t.Fatal("the number itself must not be credited with charges Meta did not assign to it")
	}
	if rep.Totals.MetaCostKES == nil || *rep.Totals.MetaCostKES != 100 {
		t.Fatalf("total in KES %v, want 100", rep.Totals.MetaCostKES)
	}
}

func TestParseRangeDefaultsToThisMonth(t *testing.T) {
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	rng, err := ParseRange("", "", now)
	if err != nil || rng.FromString() != "2026-11-01" || rng.ToString() != "2026-11-13" {
		t.Fatalf("default range %v %v", rng, err)
	}
	for _, c := range [][2]string{{"2026-11-10", "2026-11-01"}, {"x", ""}, {"2024-01-01", "2026-01-02"}} {
		if _, err := ParseRange(c[0], c[1], now); err == nil {
			t.Errorf("%v should be refused", c)
		}
	}
}
