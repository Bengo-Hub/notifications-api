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

// The WhatsApp job alert never sends an empty parameter (Meta rejects it) and its button opens the
// job under the tenant's slug on the shared rider app.
func TestRiderJobWhatsAppParams(t *testing.T) {
	d := riderJobData(map[string]interface{}{"order_number": "000014", "rider_name": "Phelix", "pickup_name": "Urban Loft Cafe Busia"}, "urban-loft", "https://riderapp.codevertexafrica.com")
	got := riderJobWhatsAppParams(d)
	want := []string{"Phelix", "000014", "Urban Loft Cafe Busia", "None"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("params = %v, want %v", got, want)
		}
	}
	if d["url"] != "/urban-loft/active" {
		t.Errorf("button path = %v, want /urban-loft/active", d["url"])
	}
	bare := riderJobWhatsAppParams(riderJobData(map[string]interface{}{}, "", "https://r.example.com"))
	for i, p := range bare {
		if p == "" {
			t.Errorf("param %d is empty", i)
		}
	}
}
