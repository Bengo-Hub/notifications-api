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
