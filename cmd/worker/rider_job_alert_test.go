package main

import "testing"

func TestRiderJobData(t *testing.T) {
	d := riderJobData(map[string]interface{}{
		"order_number":     "DL-1042",
		"pickup_name":      "Urban Loft Westlands",
		"rider_name":       "Otieno",
		"cash_on_delivery": 850.0,
	}, "urban-loft", "https://riderapp.example.com/")

	if d["url"] != "/urban-loft/active" {
		t.Fatalf("push click must open the job inside the rider app, got %v", d["url"])
	}
	if d["job_link"] != "https://riderapp.example.com/urban-loft/active" {
		t.Fatalf("email link wrong: %v", d["job_link"])
	}
	if d["cash_on_delivery"] != "850.00" || d["name"] != "Otieno" || d["order_number"] != "DL-1042" {
		t.Fatalf("unexpected data: %#v", d)
	}

	// No order number: fall back to the tracking code; no COD: no collect line.
	d = riderJobData(map[string]interface{}{"tracking_code": "TRK-9"}, "", "https://r.example.com")
	if d["order_number"] != "TRK-9" || d["cash_on_delivery"] != "" || d["name"] != "there" || d["url"] != "/active" {
		t.Fatalf("fallbacks wrong: %#v", d)
	}
}
