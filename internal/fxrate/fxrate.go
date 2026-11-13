// Package fxrate reads exchange rates from treasury-api, the platform's one source of rates
// (fetched daily from the configured forex provider and cached there). notifications-api uses it
// to show Meta's WhatsApp charges in KES; it never keeps a rate of its own. Same approach as
// marketflow-ai's internal/fxrate.
package fxrate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// CacheFor is how long a fetched rate is reused; treasury updates its rates daily.
const CacheFor = time.Hour

// A KES per USD outside this range is a misconfiguration (for example no USD rate in treasury,
// which returns the amount unchanged), never a real rate.
const minKESPerUSD, maxKESPerUSD = 50, 500

// Source reads rates from treasury-api over S2S.
type Source struct {
	base, key, tenant string
	rdb               redis.UniversalClient
	http              *http.Client
}

// New creates a source. rdb may be nil (no cache, every call asks treasury).
func New(treasuryURL, serviceKey, platformTenantSlug string, rdb redis.UniversalClient) *Source {
	return &Source{base: strings.TrimRight(treasuryURL, "/"), key: serviceKey, tenant: platformTenantSlug,
		rdb: rdb, http: &http.Client{Timeout: 5 * time.Second}}
}

// KESPer is how many KES one unit of currency buys: cached for an hour, else fetched from
// treasury, else the last good rate. ok is false when no rate is known.
func (s *Source) KESPer(ctx context.Context, currency string) (float64, bool) {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	if cur == "KES" {
		return 1, true
	}
	if s == nil || s.base == "" || s.key == "" || cur == "" {
		return 0, false
	}
	fresh := "notifications:fx:" + cur + ":KES"
	last := fresh + ":last"
	if r, ok := s.cached(ctx, fresh, cur); ok {
		return r, true
	}
	if r, err := s.fetch(ctx, cur); err == nil {
		if s.rdb != nil {
			v := strconv.FormatFloat(r, 'f', -1, 64)
			_ = s.rdb.Set(ctx, fresh, v, CacheFor).Err()
			_ = s.rdb.Set(ctx, last, v, 0).Err()
		}
		return r, true
	}
	return s.cached(ctx, last, cur)
}

func plausible(cur string, r float64) bool {
	if cur == "USD" {
		return r >= minKESPerUSD && r <= maxKESPerUSD
	}
	return r > 0
}

func (s *Source) cached(ctx context.Context, key, cur string) (float64, bool) {
	if s.rdb == nil {
		return 0, false
	}
	v, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return 0, false
	}
	r, err := strconv.ParseFloat(v, 64)
	return r, err == nil && plausible(cur, r)
}

func (s *Source) fetch(ctx context.Context, cur string) (float64, error) {
	q := url.Values{"from": {cur}, "to": {"KES"}, "amount": {"1"}}
	u := fmt.Sprintf("%s/api/v1/s2s/%s/exchange-rates/convert?%s", s.base, url.PathEscape(s.tenant), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-API-Key", s.key)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("treasury convert: status %d", resp.StatusCode)
	}
	var out struct {
		Converted json.Number `json:"converted"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	r, err := out.Converted.Float64()
	if err != nil {
		return 0, err
	}
	if !plausible(cur, r) {
		return 0, fmt.Errorf("treasury convert: implausible KES per %s %v", cur, r)
	}
	return r, nil
}
