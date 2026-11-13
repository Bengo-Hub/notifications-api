// Package wacost builds the platform-owner WhatsApp cost report: per WhatsApp Business Account
// we send from and per phone number on it, what Meta charged (pricing_analytics) next to what
// the tenant paid us in WhatsApp credits (subscriptions-api), so the owner sees margin per tenant.
package wacost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/modules/credits"
	"github.com/bengobox/notifications-api/internal/providers/whatsapp"
)

// Report categories, in display order. Meta's categories fold into these (MARKETING_LITE into
// marketing, AUTHENTICATION_INTERNATIONAL into authentication); anything else is "other".
const (
	CategoryMarketing      = "marketing"
	CategoryUtility        = "utility"
	CategoryAuthentication = "authentication"
	CategoryService        = "service"
	CategoryOther          = "other"
	// CategoryLegacyAllowance is the retired tiers' shared allowance, which has no category.
	CategoryLegacyAllowance = "legacy_allowance"
)

// MaxRangeDays bounds one report. Meta keeps pricing analytics for one year.
const MaxRangeDays = 366

// CacheTTL is how long a complete report is reused; PartialCacheTTL applies when a WABA or
// subscriptions-api failed, so a passing outage is not shown for an hour.
const (
	CacheTTL        = time.Hour
	PartialCacheTTL = 5 * time.Minute
)

// MetaDataDelayNote is shown with every report.
const MetaDataDelayNote = "Meta's figures are approximate and the most recent day can still be filling in. " +
	"Meta's invoice is the final word."

// Account is one whatsapp/meta_cloud provider row we send from: the platform tenant's own row,
// or a tenant's. A tenant whose number sits inside the platform's WABA has that WABA's ID and
// uses the platform token (token_source=platform, or no token of its own).
type Account struct {
	TenantID      string
	IsPlatform    bool
	WABAID        string
	PhoneNumberID string
	AccessToken   string
	TokenSource   string // "own" or "platform"
	APIVersion    string
}

// AccountSource lists the accounts, each with its token already resolved by the provider
// manager's managed link rule (the platform's token for the platform row and managed links).
type AccountSource interface {
	WhatsAppCostAccounts(ctx context.Context) ([]Account, error)
}

// UsageSource reads per tenant WhatsApp credits for a date range (subscriptions-api).
type UsageSource interface {
	WhatsAppUsage(ctx context.Context, from, to string) (credits.WhatsAppUsage, error)
}

// MetaSource is the part of the Graph API the report reads.
type MetaSource interface {
	PhoneNumbers(ctx context.Context, version, wabaID, token string) ([]PhoneNumber, error)
	PricingAnalytics(ctx context.Context, version, wabaID, token string, start, end time.Time) (Pricing, error)
}

// Cache stores rendered reports.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration)
}

// SlugResolver names tenants by ID (local tenant table). Missing IDs are left out.
type SlugResolver func(ctx context.Context, tenantIDs []string) map[string]string

// Rates converts to KES (treasury-api through internal/fxrate). ok is false when no rate is known.
type Rates interface {
	KESPer(ctx context.Context, currency string) (float64, bool)
}

// Service builds reports.
type Service struct {
	accounts AccountSource
	usage    UsageSource
	meta     MetaSource
	cache    Cache
	slugs    SlugResolver
	rates    Rates
	log      *zap.Logger

	// noPhoneLogged keeps "Meta gave no phone numbers" to one log line per WABA.
	noPhoneLogged sync.Map

	// MetaTimeout bounds each Meta call; ReportTimeout bounds the whole build.
	MetaTimeout   time.Duration
	ReportTimeout time.Duration
	now           func() time.Time
}

// NewService creates the report service. cache and slugs may be nil.
func NewService(accounts AccountSource, usage UsageSource, meta MetaSource, cache Cache, slugs SlugResolver, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{
		accounts: accounts, usage: usage, meta: meta, cache: cache, slugs: slugs, log: log.Named("wacost"),
		MetaTimeout: 20 * time.Second, ReportTimeout: 60 * time.Second, now: time.Now,
	}
}

// WithRates sets the KES rate source. Without one, every rate is unavailable.
func (s *Service) WithRates(r Rates) *Service {
	s.rates = r
	return s
}

// kesPer is the rate for currency, KES itself always 1.
func (s *Service) kesPer(ctx context.Context, currency string) (float64, bool) {
	if strings.EqualFold(strings.TrimSpace(currency), "KES") {
		return 1, true
	}
	if s.rates == nil || currency == "" {
		return 0, false
	}
	return s.rates.KESPer(ctx, currency)
}

// Range is a whole number of UTC days, both ends inclusive.
type Range struct {
	From time.Time
	To   time.Time
}

// ErrBadRange marks invalid dates; handlers answer 400.
var ErrBadRange = errors.New("invalid date range")

// ParseRange reads from and to (YYYY-MM-DD, UTC). Empty values mean this month so far.
func ParseRange(from, to string, now time.Time) (Range, error) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	f := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	t := today
	var err error
	if s := strings.TrimSpace(from); s != "" {
		if f, err = time.Parse("2006-01-02", s); err != nil {
			return Range{}, fmt.Errorf("%w: from must be a date like 2026-11-01", ErrBadRange)
		}
	}
	if s := strings.TrimSpace(to); s != "" {
		if t, err = time.Parse("2006-01-02", s); err != nil {
			return Range{}, fmt.Errorf("%w: to must be a date like 2026-11-30", ErrBadRange)
		}
	}
	if t.Before(f) {
		return Range{}, fmt.Errorf("%w: the end date is before the start date", ErrBadRange)
	}
	if t.Sub(f) > MaxRangeDays*24*time.Hour {
		return Range{}, fmt.Errorf("%w: pick at most %d days", ErrBadRange, MaxRangeDays)
	}
	return Range{From: f, To: t}, nil
}

// FromString and ToString format the range ends.
func (r Range) FromString() string { return r.From.Format("2006-01-02") }
func (r Range) ToString() string   { return r.To.Format("2006-01-02") }

// Usage is the messages and money of one category.
type Usage struct {
	Messages int64   `json:"messages"`
	Cost     float64 `json:"cost"`
}

// Credits is one category of what a tenant paid us.
type Credits struct {
	Consumed         int64   `json:"consumed"`
	Bought           int64   `json:"bought"`
	KESPaid          float64 `json:"kes_paid"`
	OverageKES       float64 `json:"overage_kes"`
	ConsumedValueKES float64 `json:"consumed_value_kes"`
}

// Row is one phone number (or, for a tenant that sends from the platform's number without a row
// of its own, one tenant with no number).
type Row struct {
	WABAID             string `json:"waba_id,omitempty"`
	PhoneNumberID      string `json:"phone_number_id,omitempty"`
	DisplayPhoneNumber string `json:"display_phone_number,omitempty"`
	VerifiedName       string `json:"verified_name,omitempty"`
	TenantID           string `json:"tenant_id,omitempty"`
	// Tenant is the slug, "platform" for the platform's own number, or "unassigned" for a number
	// no row claims.
	Tenant string `json:"tenant"`
	// Note explains a row that needs it in plain words.
	Note string `json:"note,omitempty"`

	MetaCurrency  string             `json:"meta_currency,omitempty"`
	Meta          map[string]Usage   `json:"meta"`
	MetaMessages  int64              `json:"meta_messages"`
	MetaCost      float64            `json:"meta_cost"`
	Credits       map[string]Credits `json:"credits"`
	CreditsUsed   int64              `json:"credits_consumed"`
	KESPaid       float64            `json:"kes_paid"`
	ConsumedValue float64            `json:"consumed_value_kes"`
	// WholeAccount marks the row holding a WABA's total when Meta did not split it by number.
	WholeAccount bool `json:"whole_account,omitempty"`
	// MetaCostKES is Meta's cost converted with treasury's rate, and MarginKES the credits value
	// less it. Both are unset when the rate is unavailable or Meta's side could not be read.
	MetaCostKES *float64 `json:"meta_cost_kes,omitempty"`
	MarginKES   *float64 `json:"margin_kes,omitempty"`
}

// WABA is one WhatsApp Business Account and its rows.
type WABA struct {
	WABAID      string `json:"waba_id"`
	Owner       string `json:"owner"`
	TokenSource string `json:"token_source"`
	Currency    string `json:"currency,omitempty"`
	// KESPerUnit is treasury's rate for Currency; RateUnavailable is set when none could be read.
	KESPerUnit      float64 `json:"kes_per_unit,omitempty"`
	RateUnavailable bool    `json:"rate_unavailable,omitempty"`
	// Error is Meta's refusal or a missing token; the rest of the report still renders.
	Error string `json:"error,omitempty"`
	Rows  []Row  `json:"rows"`
}

// Totals add up the report. MetaCost is per original currency; MetaCostKES and MarginKES are
// set only when every WABA was read and every rate was available.
type Totals struct {
	MetaMessages    int64              `json:"meta_messages"`
	MetaCost        map[string]float64 `json:"meta_cost"`
	MetaCostKES     *float64           `json:"meta_cost_kes,omitempty"`
	RateUnavailable bool               `json:"rate_unavailable,omitempty"`
	CreditsUsed     int64              `json:"credits_consumed"`
	KESPaid         float64            `json:"kes_paid"`
	ConsumedValue   float64            `json:"consumed_value_kes"`
	MarginKES       *float64           `json:"margin_kes,omitempty"`
}

// Report is the response body.
type Report struct {
	From        string    `json:"from"`
	To          string    `json:"to"`
	GeneratedAt time.Time `json:"generated_at"`
	Cached      bool      `json:"cached"`
	Categories  []string  `json:"categories"`
	WABAs       []WABA    `json:"wabas"`
	// Unbilled lists tenants with credit movement but no WhatsApp row, which send from the
	// platform's number; their Meta cost is inside the platform number's row.
	Unbilled     []Row    `json:"tenants_on_platform_number"`
	Totals       Totals   `json:"totals"`
	CreditsError string   `json:"credits_error,omitempty"`
	Notes        []string `json:"notes"`
}

// CacheKey is the Redis key of a range's report.
func CacheKey(r Range) string {
	return "notifications:whatsapp-costs:v1:" + r.FromString() + ":" + r.ToString()
}

// Build returns the report for rng, from the cache when a fresh one exists. Meta and
// subscriptions-api are called on a context detached from the request, so a closed browser tab
// does not waste the calls already made, bounded by ReportTimeout.
func (s *Service) Build(ctx context.Context, rng Range) (Report, error) {
	key := CacheKey(rng)
	if s.cache != nil {
		if raw, ok := s.cache.Get(ctx, key); ok {
			var rep Report
			if err := json.Unmarshal(raw, &rep); err == nil {
				rep.Cached = true
				return rep, nil
			}
		}
	}

	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.ReportTimeout)
	defer cancel()

	accounts, err := s.accounts.WhatsAppCostAccounts(work)
	if err != nil {
		return Report{}, fmt.Errorf("load WhatsApp accounts: %w", err)
	}

	// subscriptions-api in parallel with Meta.
	var (
		usage    credits.WhatsAppUsage
		usageErr error
		wg       sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if s.usage == nil {
			usageErr = errors.New("subscriptions-api is not configured")
			return
		}
		usage, usageErr = s.usage.WhatsAppUsage(work, rng.FromString(), rng.ToString())
	}()

	groups := groupAccounts(accounts)
	results := make([]wabaResult, len(groups))
	for i := range groups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = s.readWABA(work, groups[i], rng)
		}(i)
	}
	wg.Wait()

	rep := s.assemble(work, rng, groups, results, usage, usageErr)

	if s.cache != nil {
		ttl := CacheTTL
		if rep.CreditsError != "" || anyWABAError(rep.WABAs) {
			ttl = PartialCacheTTL
		}
		if raw, err := json.Marshal(rep); err == nil {
			s.cache.Set(ctx, key, raw, ttl)
		}
	}
	return rep, nil
}

func anyWABAError(ws []WABA) bool {
	for _, w := range ws {
		if w.Error != "" {
			return true
		}
	}
	return false
}

// wabaGroup is every account sharing one WABA, with the token used to read it.
type wabaGroup struct {
	WABAID      string
	Accounts    []Account
	Token       string
	TokenSource string
	Version     string
	HasPlatform bool
}

// groupAccounts groups accounts by WABA. Tokens arrive already resolved by the provider
// manager's managed link rule; the WABA is read with the platform row's token when it is on the
// WABA, else a managed link's (also the platform's), else the first own token.
func groupAccounts(accounts []Account) []*wabaGroup {
	byID := map[string]*wabaGroup{}
	var order []string
	for _, a := range accounts {
		id := strings.TrimSpace(a.WABAID)
		if id == "" {
			continue
		}
		g, ok := byID[id]
		if !ok {
			g = &wabaGroup{WABAID: id}
			byID[id] = g
			order = append(order, id)
		}
		g.Accounts = append(g.Accounts, a)
		if a.IsPlatform {
			g.HasPlatform = true
		}
	}
	out := make([]*wabaGroup, 0, len(order))
	for _, id := range order {
		g := byID[id]
		pick := func(ok func(Account) bool, source string) {
			if g.Token != "" {
				return
			}
			for _, a := range g.Accounts {
				if ok(a) && strings.TrimSpace(a.AccessToken) != "" {
					g.Token, g.TokenSource, g.Version = strings.TrimSpace(a.AccessToken), source, a.APIVersion
					return
				}
			}
		}
		pick(func(a Account) bool { return a.IsPlatform }, "platform")
		pick(func(a Account) bool { return a.TokenSource == "platform" }, "platform")
		pick(func(a Account) bool { return true }, "own")
		out = append(out, g)
	}
	return out
}

type wabaResult struct {
	Numbers  []PhoneNumber
	Pricing  Pricing
	Err      error
	PhoneErr error
}

// readWABA reads one WABA's numbers and pricing, each call bounded by MetaTimeout.
func (s *Service) readWABA(ctx context.Context, g *wabaGroup, rng Range) wabaResult {
	if g.Token == "" {
		return wabaResult{Err: errors.New("no Meta access token is saved for this account, so Meta's charges cannot be read")}
	}
	version := whatsapp.GraphVersion(g.Version)
	var res wabaResult

	pctx, cancel := context.WithTimeout(ctx, s.MetaTimeout)
	res.Pricing, res.Err = s.meta.PricingAnalytics(pctx, version, g.WABAID, g.Token, rng.From, rng.To.AddDate(0, 0, 1))
	cancel()
	if res.Err != nil {
		s.log.Warn("meta pricing analytics failed", zap.String("waba_id", g.WABAID), zap.Error(res.Err))
		return res
	}
	nctx, cancel := context.WithTimeout(ctx, s.MetaTimeout)
	res.Numbers, res.PhoneErr = s.meta.PhoneNumbers(nctx, version, g.WABAID, g.Token)
	cancel()
	if res.PhoneErr != nil {
		s.log.Warn("meta phone numbers failed", zap.String("waba_id", g.WABAID), zap.Error(res.PhoneErr))
	}
	return res
}

// MetaCategory folds a Meta pricing category into a report category.
func MetaCategory(c string) string {
	switch strings.ToUpper(strings.TrimSpace(c)) {
	case "MARKETING", "MARKETING_LITE":
		return CategoryMarketing
	case "UTILITY":
		return CategoryUtility
	case "AUTHENTICATION", "AUTHENTICATION_INTERNATIONAL":
		return CategoryAuthentication
	case "SERVICE":
		return CategoryService
	}
	return CategoryOther
}

// creditCategory maps a subscriptions-api wallet to a report category.
func creditCategory(service string) string {
	switch service {
	case credits.ServiceWhatsAppMarketing:
		return CategoryMarketing
	case credits.ServiceWhatsAppUtility:
		return CategoryUtility
	case credits.ServiceWhatsAppAuthentication:
		return CategoryAuthentication
	case credits.ServiceWhatsAppService:
		return CategoryService
	case "whatsapp_quota":
		return CategoryLegacyAllowance
	}
	return CategoryOther
}

func (s *Service) assemble(ctx context.Context, rng Range, groups []*wabaGroup, results []wabaResult, usage credits.WhatsAppUsage, usageErr error) Report {
	rep := Report{
		From: rng.FromString(), To: rng.ToString(), GeneratedAt: s.now().UTC(),
		Categories: []string{CategoryMarketing, CategoryUtility, CategoryAuthentication, CategoryService, CategoryOther, CategoryLegacyAllowance},
		WABAs:      []WABA{}, Unbilled: []Row{},
		Totals: Totals{MetaCost: map[string]float64{}},
		Notes: []string{
			MetaDataDelayNote,
			"Meta bills in the WhatsApp Business Account's own currency and our credits are in KES. " +
				"Meta's cost is converted to KES with treasury's current rate; where no rate is available the margin is left out.",
			"Credits paid is what tenants paid for credits bought in these dates. Credits value is the credits they spent, " +
				"priced at today's rate, plus any overage: compare that with Meta's cost.",
		},
	}
	if usageErr != nil {
		rep.CreditsError = "Could not read tenant credits from subscriptions: " + usageErr.Error()
	}

	// Tenant slugs: subscriptions-api's names first, then the local tenant table.
	slugs := map[string]string{}
	usageByTenant := map[string]credits.WhatsAppUsageTenant{}
	for _, t := range usage.Tenants {
		id := t.TenantID.String()
		usageByTenant[id] = t
		if t.Slug != "" {
			slugs[id] = t.Slug
		}
	}
	if s.slugs != nil {
		var missing []string
		for _, g := range groups {
			for _, a := range g.Accounts {
				if !a.IsPlatform && slugs[a.TenantID] == "" {
					missing = append(missing, a.TenantID)
				}
			}
		}
		if len(missing) > 0 {
			for id, slug := range s.slugs(ctx, missing) {
				slugs[id] = slug
			}
		}
	}
	tenantLabel := func(a Account) string {
		if a.IsPlatform {
			return "platform"
		}
		if s := slugs[a.TenantID]; s != "" {
			return s
		}
		return a.TenantID
	}

	// Tenants whose credits are already on a row. The platform's own sends are never billed.
	attached := map[string]bool{}
	for _, g := range groups {
		for _, a := range g.Accounts {
			if a.IsPlatform && a.TenantID != "" {
				attached[a.TenantID] = true
			}
		}
	}
	marginKnown := true
	var totalMetaKES float64

	for i, g := range groups {
		res := results[i]
		w := WABA{WABAID: g.WABAID, TokenSource: g.TokenSource, Rows: []Row{}}
		if g.HasPlatform {
			w.Owner = "platform"
		} else if len(g.Accounts) > 0 {
			w.Owner = tenantLabel(g.Accounts[0])
		}
		if res.Err != nil {
			w.Error = res.Err.Error()
			marginKnown = false
		}
		w.Currency = res.Pricing.Currency

		// One row per number: the WABA's listed numbers, plus any number a row names that the
		// listing missed, plus any number Meta billed that neither knows.
		rows := []*Row{}
		byPhoneID := map[string]*Row{}
		byDigits := map[string]*Row{}
		addRow := func(r *Row) {
			rows = append(rows, r)
			if r.PhoneNumberID != "" {
				byPhoneID[r.PhoneNumberID] = r
			}
			if d := digits(r.DisplayPhoneNumber); d != "" {
				byDigits[d] = r
			}
		}
		for _, n := range res.Numbers {
			addRow(&Row{WABAID: g.WABAID, PhoneNumberID: n.ID, DisplayPhoneNumber: n.DisplayPhoneNumber, VerifiedName: n.VerifiedName})
		}
		for _, a := range g.Accounts {
			if a.PhoneNumberID != "" && byPhoneID[a.PhoneNumberID] == nil {
				addRow(&Row{WABAID: g.WABAID, PhoneNumberID: a.PhoneNumberID})
			}
		}
		// Owner of each number.
		for _, r := range rows {
			for _, a := range g.Accounts {
				if a.PhoneNumberID != "" && a.PhoneNumberID == r.PhoneNumberID {
					// A tenant row on the platform's WABA wins over the platform's own row only
					// when it names the number, which is what this match is.
					if r.Tenant == "" || r.Tenant == "platform" {
						r.Tenant, r.TenantID = tenantLabel(a), a.TenantID
					}
				}
			}
			if r.Tenant == "" {
				switch {
				case g.HasPlatform:
					r.Tenant = "platform"
				case len(g.Accounts) == 1:
					r.Tenant, r.TenantID = tenantLabel(g.Accounts[0]), g.Accounts[0].TenantID
				default:
					r.Tenant = "unassigned"
					r.Note = "No tenant has this number saved."
				}
			}
		}

		// Meta's charges per number. A data point without a phone number (Meta did not split
		// by number) goes to one "whole account" row for the WABA, so the report still renders.
		var whole *Row
		for _, dp := range res.Pricing.DataPoints {
			d := digits(dp.PhoneNumber)
			r := byDigits[d]
			if r == nil && d == "" {
				if whole == nil {
					whole = &Row{WABAID: g.WABAID, Tenant: w.Owner, WholeAccount: true,
						Note: "Whole account: Meta did not split these charges by number."}
					if !g.HasPlatform && len(g.Accounts) == 1 {
						whole.TenantID = g.Accounts[0].TenantID
					}
					rows = append(rows, whole)
					if _, seen := s.noPhoneLogged.LoadOrStore(g.WABAID, true); !seen {
						s.log.Warn("meta pricing analytics has no phone numbers; showing the whole account total",
							zap.String("waba_id", g.WABAID))
					}
				}
				r = whole
			}
			if r == nil {
				r = &Row{WABAID: g.WABAID, DisplayPhoneNumber: dp.PhoneNumber, Tenant: "unassigned",
					Note: "Meta billed this number but it is not on the account any more."}
				addRow(r)
			}
			if r.Meta == nil {
				r.Meta = map[string]Usage{}
			}
			cat := MetaCategory(dp.PricingCategory)
			u := r.Meta[cat]
			u.Messages += dp.Volume
			u.Cost += dp.Cost
			r.Meta[cat] = u
			r.MetaMessages += dp.Volume
			r.MetaCost += dp.Cost
		}
		if res.PhoneErr != nil && res.Err == nil {
			w.Error = "Meta's charges were read but the number list was not, so numbers may show without a name: " + res.PhoneErr.Error()
		}

		// Treasury's rate for the WABA's currency, once per WABA (the source caches it).
		rate, rateOK := 0.0, false
		if res.Err == nil {
			rate, rateOK = s.kesPer(ctx, res.Pricing.Currency)
			if rateOK {
				w.KESPerUnit = rate
			} else {
				w.RateUnavailable = true
				rep.Totals.RateUnavailable = true
			}
		}

		for _, r := range rows {
			r.MetaCurrency = res.Pricing.Currency
			if r.Meta == nil {
				r.Meta = map[string]Usage{}
			}
			r.Credits = map[string]Credits{}
			if r.TenantID != "" && r.Tenant != "platform" && !attached[r.TenantID] {
				if t, ok := usageByTenant[r.TenantID]; ok {
					applyCredits(r, t)
				}
				attached[r.TenantID] = true
			}
			if res.Err == nil && rateOK {
				kes := r.MetaCost * rate
				m := r.ConsumedValue - kes
				r.MetaCostKES, r.MarginKES = &kes, &m
				totalMetaKES += kes
			}
			rep.Totals.MetaMessages += r.MetaMessages
			if r.MetaCost != 0 || r.MetaCurrency != "" {
				rep.Totals.MetaCost[currencyOr(r.MetaCurrency)] += r.MetaCost
			}
			rep.Totals.CreditsUsed += r.CreditsUsed
			rep.Totals.KESPaid += r.KESPaid
			rep.Totals.ConsumedValue += r.ConsumedValue
			w.Rows = append(w.Rows, *r)
		}
		rep.WABAs = append(rep.WABAs, w)
	}

	// Tenants with credit movement and no number of their own send from the platform's number.
	ids := make([]string, 0, len(usageByTenant))
	for id := range usageByTenant {
		if !attached[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := usageByTenant[id]
		label := t.Slug
		if label == "" {
			label = id
		}
		r := Row{TenantID: id, Tenant: label, Meta: map[string]Usage{}, Credits: map[string]Credits{},
			Note: "No number of its own: Meta's charge for these messages is in the platform number's row."}
		applyCredits(&r, t)
		if r.CreditsUsed == 0 && r.KESPaid == 0 && r.ConsumedValue == 0 {
			continue
		}
		rep.Totals.CreditsUsed += r.CreditsUsed
		rep.Totals.KESPaid += r.KESPaid
		rep.Totals.ConsumedValue += r.ConsumedValue
		rep.Unbilled = append(rep.Unbilled, r)
	}

	// A total in KES needs every WABA read and converted; the margin also needs our credits.
	if marginKnown && !rep.Totals.RateUnavailable && len(rep.WABAs) > 0 {
		kes := totalMetaKES
		rep.Totals.MetaCostKES = &kes
		if usageErr == nil {
			m := rep.Totals.ConsumedValue - kes
			rep.Totals.MarginKES = &m
		}
	}
	return rep
}

func currencyOr(c string) string {
	if c == "" {
		return "UNKNOWN"
	}
	return c
}

// applyCredits copies a tenant's credit lines onto a row.
func applyCredits(r *Row, t credits.WhatsAppUsageTenant) {
	if r.Credits == nil {
		r.Credits = map[string]Credits{}
	}
	for _, c := range t.Categories {
		cat := creditCategory(c.Service)
		cur := r.Credits[cat]
		cur.Consumed += c.UnitsConsumed
		cur.Bought += c.UnitsBought
		cur.KESPaid += c.KESPaid
		cur.OverageKES += c.OverageKES
		cur.ConsumedValueKES += c.ConsumedValueKES
		r.Credits[cat] = cur
		r.CreditsUsed += c.UnitsConsumed
		r.KESPaid += c.KESPaid
		r.ConsumedValue += c.ConsumedValueKES
	}
}
