package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The shared service key reads the full spec (MarketFlow's endpoint discovery); anonymous callers
// and wrong keys keep the external subset. A nil validator means no App secret is checked.
func TestOpenAPIJSONInternalKeyGetsTheFullSpec(t *testing.T) {
	h, err := NewSwaggerHandler(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.WithInternalKey("svc-key")
	get := func(key string) (string, int) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/openapi.json", nil)
		if key != "" {
			r.Header.Set("X-API-Key", key)
		}
		w := httptest.NewRecorder()
		h.OpenAPIJSON(w, r)
		var doc struct {
			Paths map[string]any `json:"paths"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		return w.Header().Get("X-Docs-View"), len(doc.Paths)
	}
	view, public := get("")
	if view != "external" {
		t.Fatalf("anonymous view %q", view)
	}
	if view, n := get("wrong"); view != "external" || n != public {
		t.Fatalf("wrong key: %q %d paths", view, n)
	}
	view, full := get("svc-key")
	if view != "internal" || full <= public {
		t.Fatalf("service key: %q, %d paths against %d public", view, full, public)
	}

	h.WithInternalKey("")
	if view, _ := get("svc-key"); view != "external" {
		t.Fatal("with no key configured nobody gets the full spec")
	}
}
