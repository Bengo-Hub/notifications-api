package main

import "testing"

func TestReviewEmailCarriesGoogleReviewURL(t *testing.T) {
	data := map[string]interface{}{
		"order_id":          "o1",
		"tenant_slug":       "demo",
		"google_review_url": "https://search.google.com/local/writereview?placeid=ChIJN1t_tDeuEmsRUsoyG83frY4",
	}
	got := reviewEmailDataBuilder(data, "https://ordering.example.com")
	if got["google_review_url"] != data["google_review_url"] {
		t.Fatalf("google_review_url = %v", got["google_review_url"])
	}
	if got["review_link"] != "https://ordering.example.com/demo/orders/guest/o1?rate=1" {
		t.Fatalf("review_link = %v", got["review_link"])
	}
}

func TestReviewEmailDropsUnsafeGoogleReviewURL(t *testing.T) {
	for _, v := range []interface{}{nil, "", "javascript:alert(1)", "http://g.page/r/x", 42} {
		got := reviewEmailDataBuilder(map[string]interface{}{"order_id": "o1", "google_review_url": v}, "https://ordering.example.com")
		if got["google_review_url"] != "" {
			t.Fatalf("%v: google_review_url = %v, want empty", v, got["google_review_url"])
		}
	}
}
