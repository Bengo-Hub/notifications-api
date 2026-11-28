// Package erpcalendar reads the platform's public holiday calendar from erp-api (S2S), the one
// place a gazetted holiday is entered. Occasions use it to fill a year's date the catalogue does
// not have yet (Idd-ul-Fitr, Idd-ul-Azha).
package erpcalendar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	okTTL   = 12 * time.Hour   // a good answer is reused for half a day
	failTTL = 30 * time.Minute // after a failure the ERP is not asked again for this long
	timeout = 5 * time.Second
)

// Source answers from erp-api's GET /api/v1/hrm/leave/holidays as the platform's system tenant.
type Source struct {
	base, key, tenant string
	http              *http.Client
	mu                sync.Mutex
	cache             map[int]entry
	now               func() time.Time
}

type entry struct {
	until time.Time
	dates map[string]string
}

// New returns nil when the URL, key or tenant is missing, which switches the lookup off.
func New(base, key, tenant string) *Source {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || key == "" || tenant == "" {
		return nil
	}
	return &Source{base: base, key: key, tenant: tenant, http: &http.Client{Timeout: timeout}, cache: map[int]entry{}, now: time.Now}
}

// Dates is the first date (YYYY-MM-DD) of each keyed platform holiday in year. A failure gives an
// empty map and is remembered for failTTL, so a slow or absent ERP never holds up occasions.
func (s *Source) Dates(ctx context.Context, year int) map[string]string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	e, ok := s.cache[year]
	s.mu.Unlock()
	if ok && s.now().Before(e.until) {
		return e.dates
	}
	dates, err := s.fetch(ctx, year)
	ttl := okTTL
	if err != nil {
		dates, ttl = map[string]string{}, failTTL
	}
	s.mu.Lock()
	s.cache[year] = entry{until: s.now().Add(ttl), dates: dates}
	s.mu.Unlock()
	return dates
}

func (s *Source) fetch(ctx context.Context, year int) (map[string]string, error) {
	u := s.base + "/api/v1/hrm/leave/holidays?limit=200&year=" + strconv.Itoa(year)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", s.key)
	req.Header.Set("X-Tenant-ID", s.tenant)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("erp holidays: status %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			Key    string `json:"key"`
			Date   string `json:"date"`
			Active bool   `json:"active"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("erp holidays: decode: %w", err)
	}
	out := map[string]string{}
	for _, h := range body.Data {
		if h.Key == "" || !h.Active || len(h.Date) < 10 {
			continue
		}
		d := h.Date[:10]
		if cur, ok := out[h.Key]; !ok || d < cur {
			out[h.Key] = d
		}
	}
	return out, nil
}
