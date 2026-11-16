package whatsapp

import (
	"reflect"
	"testing"
)

// A rewritten template ships before Meta approves it: until then the send falls back to the
// approved template it replaces, with that template's own parameter order.
func TestSupersededFallback(t *testing.T) {
	md := map[string]interface{}{
		"template_name":   "hadia_change_requested_v2_btn",
		"template_params": []string{"Amina", "Our wedding", "Tom", "publish the registry"},
	}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "hadia_change_requested_v1_btn" {
		t.Fatalf("fallback = %v", md["template_fallback_name"])
	}
	want := []string{"Amina", "Tom", "publish the registry", "Our wedding"}
	if got := md["template_fallback_params"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback params = %v, want %v", got, want)
	}

	// Same order: the params pass through.
	md = map[string]interface{}{"template_name": "credits_balance_low_v2_btn", "template_params": []string{"a", "b", "c", "d"}, "template_button_param": "billing"}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "credits_balance_low_v1_btn" || !reflect.DeepEqual(md["template_fallback_params"], []string{"a", "b", "c", "d"}) ||
		md["template_fallback_button_param"] != "billing" {
		t.Fatalf("got %v", md)
	}

	// A fallback the caller set is kept.
	md = map[string]interface{}{"template_name": "credits_balance_low_v2_btn", "template_fallback_name": "other"}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "other" {
		t.Fatalf("caller fallback replaced: %v", md)
	}

	// The test template's v2 has its own params; v1 had none.
	md = map[string]interface{}{"template_name": "notifications_test_message_v2", "template_params": []string{"16 Nov", "T-1"}}
	addSupersededFallback(md)
	if md["template_fallback_name"] != "notifications_test_message_v1" || len(md["template_fallback_params"].([]string)) != 0 {
		t.Fatalf("got %v", md)
	}
}
