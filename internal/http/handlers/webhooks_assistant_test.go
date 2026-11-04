package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
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

// Only an ad referral with a click id on a tenant number is handed over; the guards return before
// any publish, so a nil connection is safe.
func TestPublishAdReferralGuards(t *testing.T) {
	h := &WebhookHandler{log: zap.NewNop()}
	m := waInboundMessage{From: "254712345678", ID: "wamid.2", Type: "text"}
	h.publishAdReferral(context.Background(), "", "pn1", m, "")
	h.publishAdReferral(context.Background(), uuid.NewString(), "pn1", m, "")
	if m.Referral != nil {
		t.Fatal("an organic message has no referral")
	}
	var payload waInboundMessage
	_ = json.Unmarshal([]byte(`{"from":"254712345678","id":"w","type":"text","referral":{"source_type":"ad","source_id":"120","ctwa_clid":"ARAkL"}}`), &payload)
	if payload.Referral == nil || payload.Referral.CtwaClid != "ARAkL" || payload.Referral.SourceID != "120" {
		t.Fatalf("referral parse: %+v", payload.Referral)
	}
}
