package main

import (
	"fmt"
	"strconv"
	"strings"
)

// orderDeliveryDetails turns the delivery keys of an ordering.order.created event into the
// template values of the order confirmation email: where it goes, what it costs and how that
// was worked out. Nothing is returned for orders without a delivery (pickup, dine-in), so the
// template shows no delivery block.
//
// A delivery priced by distance (outside the named areas) arrives with
// delivery_needs_confirmation, which switches on the "please check these details" paragraph:
// the customer is told the area, distance and fee and who to contact if they are wrong.
func orderDeliveryDetails(data map[string]interface{}) map[string]interface{} {
	method, hasMethod := data["delivery_method"].(string)
	_, hasFee := data["delivery_fee"].(float64)
	if !hasMethod && !hasFee {
		return nil
	}
	free, _ := data["delivery_free"].(bool)
	fee := "Free"
	if !free {
		fee = formatMoney(data["delivery_fee"], data["currency"])
	}
	out := map[string]interface{}{
		"delivery_fee_text": fee,
		"delivery_confirm":  data["delivery_needs_confirmation"] == true,
	}
	if area, _ := data["delivery_area"].(string); strings.TrimSpace(area) != "" {
		out["delivery_area"] = strings.TrimSpace(area)
	}
	switch method {
	case "per_km":
		out["delivery_priced_by"] = "Distance from the outlet"
	case "zone":
		out["delivery_priced_by"] = "Delivery area fee"
	}
	if km, _ := data["delivery_distance_km"].(float64); km > 0 {
		n := strconv.FormatFloat(km, 'f', -1, 64)
		if t, _ := data["delivery_distance_type"].(string); t == "road" {
			out["delivery_distance_text"] = n + " km by road"
		} else {
			out["delivery_distance_text"] = "about " + n + " km"
		}
	}
	if eta, _ := data["delivery_eta_minutes"].(float64); eta > 0 {
		out["delivery_eta_text"] = fmt.Sprintf("about %d minutes", int(eta))
	}
	return out
}

// mergeDetails copies extra into base (extra wins) and returns base.
func mergeDetails(base, extra map[string]interface{}) map[string]interface{} {
	for k, v := range extra {
		base[k] = v
	}
	return base
}
