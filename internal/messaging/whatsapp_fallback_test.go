package messaging

import (
	"encoding/json"
	"testing"
)

func TestNextFallbackWalksTheNumbersInOrder(t *testing.T) {
	msg := Message{To: []string{"admin"}, IdempotencyKey: "k", Metadata: map[string]any{
		"template_name": "t", MetaSentMessageID: "wamid.1", MetaFallbackTo: []string{"outlet", "tenant"},
	}}
	next, ok := NextFallback(msg)
	if !ok || next.To[0] != "outlet" || next.IdempotencyKey != "k-fb-outlet" {
		t.Fatalf("first fallback = %+v", next)
	}
	if _, has := next.Metadata[MetaSentMessageID]; has {
		t.Fatal("the next attempt must not carry the failed send's message id")
	}
	if msg.Metadata[MetaSentMessageID] != "wamid.1" {
		t.Fatal("the original message must not change")
	}

	// After the JSON round trip through Redis, the rest still reads.
	b, _ := json.Marshal(next)
	var parked Message
	_ = json.Unmarshal(b, &parked)
	last, ok := NextFallback(parked)
	if !ok || last.To[0] != "tenant" {
		t.Fatalf("second fallback = %+v", last)
	}
	if _, ok := NextFallback(last); ok {
		t.Fatal("no number left after the tenant phone")
	}
}
