package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

func createdEvent(extra map[string]interface{}) map[string]interface{} {
	d := map[string]interface{}{
		"order_id": "o1", "order_number": "ORD-1", "customer_name": "Amina", "total_amount": 1380.0, "currency": "KES",
		"tenant_slug": "urban-loft",
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

func TestPickupOrdersGetNoDeliveryBlock(t *testing.T) {
	if got := orderDeliveryDetails(createdEvent(nil)); got != nil {
		t.Fatalf("a pickup order has no delivery details, got %v", got)
	}
}

func TestDistancePricedDeliveryAsksTheCustomerToCheckIt(t *testing.T) {
	got := orderDeliveryDetails(createdEvent(map[string]interface{}{
		"delivery_fee": 380.0, "delivery_free": false, "delivery_method": "per_km", "delivery_area": "",
		"delivery_distance_km": 7.4, "delivery_distance_type": "road", "delivery_eta_minutes": 40.0,
		"delivery_needs_confirmation": true,
	}))
	if got["delivery_fee_text"] != "KES 380" || got["delivery_confirm"] != true {
		t.Fatalf("details = %v", got)
	}
	if got["delivery_distance_text"] != "7.4 km by road" || got["delivery_priced_by"] != "Distance from the outlet" || got["delivery_eta_text"] != "about 40 minutes" {
		t.Fatalf("details = %v", got)
	}
	if _, ok := got["delivery_area"]; ok {
		t.Fatal("an unnamed place has no area line")
	}
}

func TestAreaPricedAndFreeDeliveriesNeedNoConfirmation(t *testing.T) {
	area := orderDeliveryDetails(createdEvent(map[string]interface{}{
		"delivery_fee": 100.0, "delivery_method": "zone", "delivery_area": " Alupe ", "delivery_distance_km": 4.0, "delivery_distance_type": "estimated",
	}))
	if area["delivery_area"] != "Alupe" || area["delivery_confirm"] != false || area["delivery_distance_text"] != "about 4 km" || area["delivery_priced_by"] != "Delivery area fee" {
		t.Fatalf("area details = %v", area)
	}
	free := orderDeliveryDetails(createdEvent(map[string]interface{}{"delivery_fee": 0.0, "delivery_free": true, "delivery_method": "zone", "delivery_area": "Busia Town"}))
	if free["delivery_fee_text"] != "Free" {
		t.Fatalf("free details = %v", free)
	}
}

func TestOrderPlacedEmailRendersTheDeliveryBlockOnlyWhenThereIsOne(t *testing.T) {
	tpl, err := template.ParseFiles("../../templates/email/ordering/order_placed.html")
	if err != nil {
		t.Fatal(err)
	}
	render := func(d map[string]interface{}) string {
		var out bytes.Buffer
		if err := tpl.ExecuteTemplate(&out, "content", d); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	build := orderMappings["ordering.order.created"].DataBuilder

	distance := render(build(createdEvent(map[string]interface{}{
		"delivery_address": "Sega market", "delivery_fee": 380.0, "delivery_method": "per_km", "delivery_distance_km": 7.4,
		"delivery_distance_type": "road", "delivery_needs_confirmation": true,
	}), "https://ordering.example.com"))
	for _, want := range []string{"Delivery details", "KES 380", "7.4 km by road", "Sega market", "Please check the address and the fee"} {
		if !strings.Contains(distance, want) {
			t.Errorf("distance-priced email lacks %q", want)
		}
	}

	area := render(build(createdEvent(map[string]interface{}{"delivery_fee": 100.0, "delivery_method": "zone", "delivery_area": "Alupe"}), "https://ordering.example.com"))
	if !strings.Contains(area, "Alupe") || strings.Contains(area, "Please check the address and the fee") {
		t.Errorf("an area-priced email shows the area but asks for no confirmation:\n%s", area)
	}

	pickup := render(build(createdEvent(nil), "https://ordering.example.com"))
	if strings.Contains(pickup, "Delivery details") {
		t.Error("a pickup order must not show a delivery block")
	}
}
