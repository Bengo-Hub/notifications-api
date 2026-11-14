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

// A tap carries the id we gave the button or row; typed text has none.
func TestInboundReplyID(t *testing.T) {
	cases := map[string]string{
		`{"type":"text","text":{"body":"YES 1234"}}`:                                                                                    "",
		`{"type":"button","button":{"payload":"stop","text":"Stop promotions"}}`:                                                       "stop",
		`{"type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"mf:yes:run-1:1234","title":"Approve"}}}`:     "mf:yes:run-1:1234",
		`{"type":"interactive","interactive":{"type":"list_reply","list_reply":{"id":"mf:f:objective:0","title":"Sales"}}}`:           "mf:f:objective:0",
	}
	for raw, want := range cases {
		var m waInboundMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if got := inboundReplyID(m); got != want {
			t.Errorf("%s: got %q, want %q", raw, got, want)
		}
	}
	m := waInboundMessage{From: "254712345678", ID: "wamid.3", Type: "interactive"}
	m.Interactive.Type = "button_reply"
	m.Interactive.ButtonReply.ID = "mf:yes:run-1:1234"
	if p := assistantInbound("pn1", m, "Approve", ""); p["reply_id"] != "mf:yes:run-1:1234" {
		t.Fatalf("the event must carry the button id: %v", p)
	}
	if p := assistantInbound("pn1", waInboundMessage{ID: "w", Type: "text"}, "hi", ""); p["reply_id"] != nil {
		t.Fatalf("typed text has no reply id: %v", p)
	}
}
