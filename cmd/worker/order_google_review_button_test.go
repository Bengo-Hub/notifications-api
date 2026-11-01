package main

import (
	"reflect"
	"testing"
)

func TestCompletedOrderGetsGoogleReviewButtonWhenLinkSet(t *testing.T) {
	m := orderMappings["ordering.order.completed"]
	data := map[string]interface{}{
		"name": "Amina", "order_number": "000009",
		"review_link":       "https://ordering.codevertexafrica.com/codevertex-demo/orders/guest/abc?rate=1",
		"google_review_url": "https://search.google.com/local/writereview?placeid=X",
	}
	out := whatsAppTemplateMetadata(m, data)
	if out["template_name"] != "ordering_order_completed_v2_btn" {
		t.Fatalf("template = %v, want the two-button variant", out["template_name"])
	}
	want := []string{"codevertex-demo/orders/guest/abc?rate=1", "codevertex-demo/review/google"}
	if got := out["template_button_params"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("button params = %v, want %v", got, want)
	}
	if out["template_fallback_name"] != "ordering_order_completed_v1_btn" || out["template_fallback_button_param"] != want[0] {
		t.Fatalf("fallback = %v / %v", out["template_fallback_name"], out["template_fallback_button_param"])
	}

	delete(data, "google_review_url")
	out = whatsAppTemplateMetadata(m, data)
	if out["template_name"] != "ordering_order_completed_v1_btn" || out["template_button_params"] != nil {
		t.Fatalf("without a review link the one-button template is sent: %v", out)
	}
}
