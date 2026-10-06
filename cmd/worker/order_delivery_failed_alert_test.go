package main

import "testing"

func TestDeliveryFailedAlertData(t *testing.T) {
	d := deliveryFailedAlertData(map[string]interface{}{
		"order_id":       "order:2b1f6f1e-1111-4222-8333-444455556666",
		"order_number":   "ORD-1042",
		"failure_reason": "Customer not reachable",
		"failed_at":      "2026-10-06T10:00:00Z",
	}, "https://pos.example/acme/online-orders")
	if d["order_id"] != "2b1f6f1e-1111-4222-8333-444455556666" {
		t.Fatalf("prefix not stripped: %v", d["order_id"])
	}
	if d["order_number"] != "ORD-1042" || d["failure_reason"] != "Customer not reachable" {
		t.Fatalf("unexpected %v", d)
	}
	if d["manage_link"] != "https://pos.example/acme/online-orders" {
		t.Fatalf("link %v", d["manage_link"])
	}

	empty := deliveryFailedAlertData(map[string]interface{}{"order_id": "abc"}, "")
	if empty["failure_reason"] != "No reason was given." {
		t.Fatalf("missing reason fallback: %v", empty["failure_reason"])
	}
	if empty["order_number"] != "" {
		t.Fatalf("order number should be empty, got %v", empty["order_number"])
	}
	if none := deliveryFailedAlertData(map[string]interface{}{}, ""); none["order_id"] != "" {
		t.Fatalf("missing order id should be empty, got %v", none["order_id"])
	}
}

func TestDeliveryFailedIdempotencyKey(t *testing.T) {
	if got := deliveryFailedIdempotencyKey(map[string]interface{}{"task_id": "t1"}, "o1"); got != "delivery-failed-tenant-t1" {
		t.Fatalf("got %q", got)
	}
	got := deliveryFailedIdempotencyKey(map[string]interface{}{"failed_at": "2026-10-06T10:00:00Z"}, "o1")
	if got != "delivery-failed-tenant-o1-2026-10-06T10:00:00Z" {
		t.Fatalf("got %q", got)
	}
}
