package whatsapp

import (
	"reflect"
	"testing"
)

// withSuperseded runs a test against its own table, leaving the real one as it was.
func withSuperseded(t *testing.T, m map[string]supersededTemplate) {
	t.Helper()
	saved := superseded
	superseded = m
	t.Cleanup(func() { superseded = saved })
}

// A rewritten template ships before Meta approves it: until then the send falls back to the
// approved template it replaces, with that template's own parameter order.
func TestSupersededFallback(t *testing.T) {
	withSuperseded(t, map[string]supersededTemplate{
		"change_v2_btn": {old: "change_v1_btn", order: []int{0, 2, 3, 1}},
		"balance_v2":    {old: "balance_v1"},
		"test_v2":       {old: "test_v1", order: []int{}},
	})

	md := map[string]interface{}{
		"template_name":   "change_v2_btn",
		"template_params": []string{"Amina", "Our wedding", "Tom", "publish the registry"},
	}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "change_v1_btn" {
		t.Fatalf("fallback = %v", md["template_fallback_name"])
	}
	want := []string{"Amina", "Tom", "publish the registry", "Our wedding"}
	if got := md["template_fallback_params"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback params = %v, want %v", got, want)
	}

	// Same order: the params pass through, and so does the button suffix.
	md = map[string]interface{}{"template_name": "balance_v2", "template_params": []string{"a", "b"}, "template_button_param": "billing"}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "balance_v1" || !reflect.DeepEqual(md["template_fallback_params"], []string{"a", "b"}) ||
		md["template_fallback_button_param"] != "billing" {
		t.Fatalf("got %v", md)
	}

	// A fallback the caller set is kept.
	md = map[string]interface{}{"template_name": "balance_v2", "template_fallback_name": "other"}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "other" {
		t.Fatalf("caller fallback replaced: %v", md)
	}

	// A caller fallback that is itself a rewrite in review gives way to the primary's approved
	// predecessor, with every button value (the order review template has two URL buttons).
	withSuperseded(t, map[string]supersededTemplate{
		"review_v4_btn": {old: "review_v2_btn"},
		"order_v3_btn":  {old: "order_v1_btn"},
	})
	md = map[string]interface{}{
		"template_name":                  "review_v4_btn",
		"template_params":                []string{"Amina", "#9"},
		"template_button_param":          "shop/orders/guest/x?rate=1",
		"template_button_params":         []string{"shop/orders/guest/x?rate=1", "shop/review/google"},
		"template_fallback_name":         "order_v3_btn",
		"template_fallback_params":       []string{"Amina", "#9"},
		"template_fallback_button_param": "shop/orders/guest/x?rate=1",
	}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "review_v2_btn" || !reflect.DeepEqual(md["template_fallback_params"], []string{"Amina", "#9"}) ||
		!reflect.DeepEqual(md["template_fallback_button_params"], []string{"shop/orders/guest/x?rate=1", "shop/review/google"}) {
		t.Fatalf("got %v", md)
	}
	withSuperseded(t, map[string]supersededTemplate{
		"change_v2_btn": {old: "change_v1_btn", order: []int{0, 2, 3, 1}},
		"balance_v2":    {old: "balance_v1"},
		"test_v2":       {old: "test_v1", order: []int{}},
	})

	// An empty order sends the old template no parameters.
	md = map[string]interface{}{"template_name": "test_v2", "template_params": []string{"16 Nov", "T-1"}}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "test_v1" || len(md["template_fallback_params"].([]string)) != 0 {
		t.Fatalf("got %v", md)
	}

	// A template not in the table gets no fallback.
	md = map[string]interface{}{"template_name": "anything_v1"}
	addSupersededFallback(md)
	if _, ok := md["template_fallback_name"]; ok {
		t.Fatalf("unexpected fallback: %v", md)
	}
}

// The button rewrites of 17 November 2026 fall back to the approved templates, never to the first
// round's order names (delivered v5/v6, completed v3/v4) that Meta did not approve. The two-button
// order template's caller fallback (the new one-button template) gives way to the approved
// two-button template, with both buttons.
func TestButtonRewritesFallBackToApprovedTemplates(t *testing.T) {
	for rewrite, approved := range map[string]string{
		"ordering_order_delivered_v7_btn":    "ordering_order_delivered_v3_btn",
		"ordering_order_delivered_v8_btn":    "ordering_order_delivered_v4_btn",
		"ordering_order_completed_v5_btn":    "ordering_order_completed_v1_btn",
		"ordering_order_completed_v6_btn":    "ordering_order_completed_v2_btn",
		"codevertex_enquiry_received_v2_btn": "codevertex_enquiry_received_v1_btn",
		"provider_balance_low_v2_btn":        "provider_balance_low_v1_btn",
	} {
		if got := superseded[rewrite]; got.old != approved || got.order != nil {
			t.Errorf("%s: falls back to %+v, want %s with the same parameters", rewrite, got, approved)
		}
	}
	for _, unapproved := range []string{"ordering_order_delivered_v5_btn", "ordering_order_delivered_v6_btn",
		"ordering_order_completed_v3_btn", "ordering_order_completed_v4_btn"} {
		if _, ok := superseded[unapproved]; ok {
			t.Errorf("%s was never approved and nothing sends it; drop its entry", unapproved)
		}
	}

	buttons := []string{"shop/orders/guest/x?rate=1", "shop/review/google"}
	md := map[string]interface{}{
		"template_name":                  "ordering_order_delivered_v8_btn",
		"template_params":                []string{"Amina", "#9"},
		"template_button_param":          buttons[0],
		"template_button_params":         buttons,
		"template_fallback_name":         "ordering_order_delivered_v7_btn",
		"template_fallback_params":       []string{"Amina", "#9"},
		"template_fallback_button_param": buttons[0],
	}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "ordering_order_delivered_v4_btn" ||
		!reflect.DeepEqual(md["template_fallback_button_params"], buttons) {
		t.Fatalf("got %v", md)
	}
}
