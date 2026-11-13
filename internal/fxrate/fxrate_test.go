package fxrate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKESPer(t *testing.T) {
	answer := `{"converted":129.5}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/s2s/codevertex/exchange-rates/convert" || r.Header.Get("X-API-Key") != "k" {
			t.Errorf("unexpected call %s", r.URL.Path)
		}
		if r.URL.Query().Get("from") != "USD" || r.URL.Query().Get("to") != "KES" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	s := New(srv.URL, "k", "codevertex", nil)
	ctx := context.Background()

	if r, ok := s.KESPer(ctx, "KES"); !ok || r != 1 {
		t.Fatalf("KES rate %v %v", r, ok)
	}
	if r, ok := s.KESPer(ctx, "usd"); !ok || r != 129.5 {
		t.Fatalf("USD rate %v %v", r, ok)
	}
	// Treasury without a USD rate returns the amount unchanged: that is not a rate.
	answer = `{"converted":1}`
	if _, ok := s.KESPer(ctx, "USD"); ok {
		t.Fatal("an implausible rate must read as unavailable")
	}
	if _, ok := New("", "", "codevertex", nil).KESPer(ctx, "USD"); ok {
		t.Fatal("no treasury configured means no rate")
	}
}
