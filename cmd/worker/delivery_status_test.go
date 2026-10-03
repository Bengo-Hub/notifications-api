package main

import (
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
)

func TestWithDeliveryCorrelation(t *testing.T) {
	m := withDeliveryCorrelation(nil, "treasury", "invoice", "inv-1")
	if m[metaSourceService] != "treasury" || m[metaReferenceType] != "invoice" || m[metaReferenceID] != "inv-1" {
		t.Fatalf("meta = %v", m)
	}
	keep := withDeliveryCorrelation(map[string]any{"subject": "Invoice"}, "treasury", "invoice", "")
	if _, tagged := keep[metaReferenceID]; tagged || keep["subject"] != "Invoice" {
		t.Fatalf("an empty reference must not tag the message: %v", keep)
	}
	// Uncorrelated messages and a missing connection publish nothing (and do not panic).
	publishDeliveryStatus(nil, messaging.Message{Metadata: m}, "sent", nil, zap.NewNop())
}
