package erpcalendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDates(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-API-Key") != "k" || r.Header.Get("X-Tenant-ID") != "sys" || r.URL.Query().Get("year") != "2027" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"key":"idd_ul_fitr","date":"2027-03-10T00:00:00Z","active":true},
			{"key":"idd_ul_fitr","date":"2027-03-09T00:00:00Z","active":true},
			{"key":"labour_day","date":"2027-05-01T00:00:00Z","active":false},
			{"key":"","date":"2027-03-01T00:00:00Z","active":true}]}`))
	}))
	defer srv.Close()

	if New("", "k", "sys") != nil || New(srv.URL, "", "sys") != nil {
		t.Fatal("a missing URL or key must switch the lookup off")
	}
	var off *Source
	if off.Dates(context.Background(), 2027) != nil {
		t.Fatal("nil source")
	}

	s := New(srv.URL+"/", "k", "sys")
	clock := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	got := s.Dates(context.Background(), 2027)
	if len(got) != 1 || got["idd_ul_fitr"] != "2027-03-09" {
		t.Fatalf("dates = %v", got)
	}
	s.Dates(context.Background(), 2027)
	if calls.Load() != 1 {
		t.Fatalf("a cached answer asked the ERP again: %d calls", calls.Load())
	}

	// After the cache ends, a failure gives nothing and is not retried for failTTL.
	clock = clock.Add(okTTL + time.Minute)
	fail.Store(true)
	if got := s.Dates(context.Background(), 2027); len(got) != 0 {
		t.Fatalf("failure = %v", got)
	}
	s.Dates(context.Background(), 2027)
	if calls.Load() != 2 {
		t.Fatalf("a failure was retried at once: %d calls", calls.Load())
	}
}
