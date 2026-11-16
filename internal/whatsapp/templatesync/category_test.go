package templatesync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Meta can approve a template under another category than the one submitted (a UTILITY template
// approved as MARKETING is billed as marketing and held back by Meta's per person marketing limit,
// error 131049). FetchExisting must read Meta's category and the one it replaced.
func TestFetchExistingReadsMetaCategory(t *testing.T) {
	var fields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields = r.URL.Query().Get("fields")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"name": "moved", "status": "APPROVED", "category": "MARKETING", "previous_category": "UTILITY"},
			{"name": "kept", "status": "APPROVED", "category": "UTILITY"},
		}})
	}))
	defer srv.Close()

	s := NewSyncer("waba", "token")
	s.GraphURL = srv.URL
	got, err := s.FetchExisting(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fields, "category") || !strings.Contains(fields, "previous_category") {
		t.Fatalf("fields asked of Meta: %q", fields)
	}
	if got["moved"].Category != "MARKETING" || got["moved"].PreviousCategory != "UTILITY" {
		t.Errorf("moved: %+v", got["moved"])
	}
	if got["kept"].Category != "UTILITY" || got["kept"].PreviousCategory != "" {
		t.Errorf("kept: %+v", got["kept"])
	}
}

func TestRecategorized(t *testing.T) {
	cases := []struct {
		declared string
		meta     ExistingTemplate
		want     bool
	}{
		{"UTILITY", ExistingTemplate{Category: "MARKETING"}, true},
		{"UTILITY", ExistingTemplate{Category: "UTILITY"}, false},
		{"utility", ExistingTemplate{Category: "UTILITY"}, false},
		{"UTILITY", ExistingTemplate{Category: "UTILITY", PreviousCategory: "MARKETING"}, false},
		{"", ExistingTemplate{Category: "MARKETING", PreviousCategory: "UTILITY"}, true},
		{"", ExistingTemplate{Category: "MARKETING"}, false},
		{"UTILITY", ExistingTemplate{}, false},
	}
	for _, c := range cases {
		if got := Recategorized(c.declared, c.meta); got != c.want {
			t.Errorf("Recategorized(%q, %+v) = %v, want %v", c.declared, c.meta, got, c.want)
		}
	}
}
