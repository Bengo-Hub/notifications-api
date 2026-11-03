package handlers

import (
	"encoding/json"
	"testing"
)

// Every inbound kind yields its visible text: typed text, a template quick-reply, and the title
// of a tapped interactive reply button or list row (Meta's documented webhook shapes).
func TestInboundText(t *testing.T) {
	cases := map[string]string{
		`{"type":"text","text":{"body":"STOP"}}`:                                                         "STOP",
		`{"type":"button","button":{"payload":"stop","text":"Stop promotions"}}`:                       "Stop promotions",
		`{"type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"approve","title":"Approve"}}}`: "Approve",
		`{"type":"interactive","interactive":{"type":"list_reply","list_reply":{"id":"tier_basic","title":"Poster","description":"2 credits"}}}`: "Poster",
	}
	for raw, want := range cases {
		var m waInboundMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if got := inboundText(m); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
}
