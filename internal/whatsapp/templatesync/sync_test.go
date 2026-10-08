package templatesync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A batch submits at most limit templates and leaves the rest queued; a create Meta refuses as
// "already exists" (an earlier reply that was lost) counts as on Meta, not as a failure; any other
// refusal carries Meta's readable message.
func TestRunBatchSubmitsInBatches(t *testing.T) {
	var creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"name": "on_meta", "status": "APPROVED"}}})
			return
		}
		creates.Add(1)
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body.Name {
		case "lost_reply":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid parameter","error_user_msg":"Content in this language already exists","code":100,"error_subcode":2388024}}`))
		case "bad_body":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid parameter","error_user_title":"Variables can't be at the start or end of the template","error_user_msg":"Variables can't be at the start or end of the template","code":100,"error_subcode":2388299}}`))
		default:
			_, _ = w.Write([]byte(`{"id":"1","status":"PENDING"}`))
		}
	}))
	defer srv.Close()

	s := NewSyncer("waba", "token")
	s.GraphURL = srv.URL
	defs := []TemplateDef{{Name: "on_meta"}, {Name: "lost_reply"}, {Name: "bad_body"}, {Name: "fresh"}, {Name: "later"}}

	results, remaining, err := s.RunBatch(context.Background(), defs, false, 3)
	if err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 3 || remaining != 1 {
		t.Fatalf("want 3 creates and 1 queued, got %d creates, %d remaining", creates.Load(), remaining)
	}
	want := map[string]Outcome{"on_meta": OutcomeSkipped, "lost_reply": OutcomeSkipped, "bad_body": OutcomeFailed, "fresh": OutcomeCreated, "later": OutcomeQueued}
	for _, r := range results {
		if want[r.Name] != r.Outcome {
			t.Errorf("%s: got %s, want %s", r.Name, r.Outcome, want[r.Name])
		}
		if r.Name == "bad_body" && r.Detail != "Variables can't be at the start or end of the template (Meta error 2388299)" {
			t.Errorf("readable Meta reason: %q", r.Detail)
		}
	}

	// A dry run never creates and reports every missing template, whatever the limit.
	creates.Store(0)
	results, remaining, _ = s.RunBatch(context.Background(), defs, true, 1)
	if creates.Load() != 0 || remaining != 0 || len(results) != len(defs) {
		t.Errorf("dry run: creates %d remaining %d results %d", creates.Load(), remaining, len(results))
	}
}
