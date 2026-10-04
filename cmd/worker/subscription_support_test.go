package main

import "testing"

func TestSupportFeeEventsAreMapped(t *testing.T) {
	for _, ev := range []string{"support_fee_invoice_generated", "support_fee_overdue", "support_fee_grace_reminder"} {
		m, ok := subscriptionMappings[ev]
		if !ok || m.TemplateID == "" || m.DataBuilder == nil {
			t.Fatalf("%s is not mapped", ev)
		}
	}
	data := subscriptionMappings["support_fee_grace_reminder"].DataBuilder(map[string]any{
		"agreement_name": "Dedicated engineer",
		"amount":         17400.0,
		"currency":       "KES",
		"days_remaining": 3,
		"pay_link":       "https://books.example/pay?x=1",
	}, "https://tenant.example")
	if data["plan_name"] != "Dedicated engineer" || data["amount"] != "KES 17400" || data["payment_link"] != "https://books.example/pay?x=1" {
		t.Fatalf("grace data %+v", data)
	}
	fallback := supportGraceData(map[string]any{}, "https://tenant.example")
	if fallback["payment_link"] != "https://tenant.example/settings/subscription" {
		t.Fatalf("fallback link %v", fallback["payment_link"])
	}
}

func TestSubscriptionIdempotencyKey(t *testing.T) {
	a := subscriptionEvent{ID: "e1", EventType: "support_fee_grace_reminder", AggregateID: "c1"}
	b := subscriptionEvent{ID: "e2", EventType: "support_fee_grace_reminder", AggregateID: "c1"}
	if subscriptionIdempotencyKey(a) == subscriptionIdempotencyKey(b) {
		t.Fatal("daily reminders for the same charge must not share a key")
	}
	inv := subscriptionEvent{ID: "e3", EventType: "support_fee_invoice_generated", AggregateID: "c1"}
	if got := subscriptionIdempotencyKey(inv); got != "subscription-support_fee_invoice_generated-c1" {
		t.Fatalf("invoice key %s", got)
	}
}
