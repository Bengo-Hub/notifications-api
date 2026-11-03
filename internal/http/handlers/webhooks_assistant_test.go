package handlers

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestAssistantInboundPayload(t *testing.T) {
	m := waInboundMessage{From: "254712345678", ID: "wamid.1", Type: "interactive"}
	p := assistantInbound("pn1", m, "Make a video ad", "Wanjiru")
	if p["wa_message_id"] != "wamid.1" || p["from"] != "254712345678" || p["text"] != "Make a video ad" ||
		p["phone_number_id"] != "pn1" || p["contact_name"] != "Wanjiru" || p["received_at"] == "" {
		t.Fatalf("payload: %v", p)
	}
	if _, ok := p["tenant_id"]; ok {
		t.Fatal("a platform-number message must not name a tenant")
	}
}

// The guards return before any lookup, so a nil client and connection are safe.
func TestPublishAssistantInboundGuards(t *testing.T) {
	h := &WebhookHandler{log: zap.NewNop()}
	m := waInboundMessage{From: "254712345678", ID: "wamid.1", Type: "text"}
	// No connection, a tenant number, empty text and STOP all skip without panicking.
	h.publishAssistantInbound(context.Background(), "", "pn1", m, "Make a video ad", "")
	h.publishAssistantInbound(context.Background(), "tenant-1", "pn1", m, "Make a video ad", "")
	h.publishAssistantInbound(context.Background(), "", "pn1", m, "", "")
	h.publishAssistantInbound(context.Background(), "", "pn1", m, "STOP", "")
	if h.isPlatformNumber(context.Background(), "pn1") {
		t.Fatal("no client means no platform number")
	}
}
