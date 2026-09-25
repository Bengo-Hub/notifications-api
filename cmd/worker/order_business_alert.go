package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// businessAlertEvents are the order events that mean the business has something to act on: an
// order waiting to be accepted (manual acceptance, the default) or, under automatic acceptance, an
// order that just went to the kitchen. An unpaid online-payment order never triggers an alert.
// sharedPOSBase is the shared POS app domain the ordering_new_order_business_v1_btn template's
// "Open Orders" button was approved against (templates.json).
const sharedPOSBase = "https://pos.codevertexafrica.com"

var businessAlertEvents = map[string]bool{
	"ordering.order.awaiting_acceptance": true,
	"ordering.order.confirmed":           true,
}

// sendBusinessNewOrderAlert tells the business about a new online order by email (tenant contact
// email) and WhatsApp (tenant contact phone, via the approved ordering_new_order_business_v1
// template). Both share one idempotency key per order, so the awaiting_acceptance alert and the
// later confirmation of the same order never alert twice.
func sendBusinessNewOrderAlert(ctx context.Context, nc *nats.Conn, cfg *config.Config, ti *tenantInfo, tenantID string, evtData map[string]interface{}, logg *zap.Logger) {
	if ti == nil {
		return
	}
	orderID, _ := evtData["order_id"].(string)
	orderNumber, _ := evtData["order_number"].(string)
	customer := waParam(evtData["customer_name"], "a customer")
	total := formatMoney(evtData["grand_total"], evtData["currency"])
	fulfilment := strings.Title(strings.ReplaceAll(fmt.Sprintf("%v", evtData["fulfillment_type"]), "_", " ")) //nolint:staticcheck // ASCII labels only
	// The POS online orders queue: the tenant's own POS domain when it has one, else the shared
	// POS app. (It used to fall back to the tenant's marketing website, which has no queue.)
	posBase := strings.TrimRight(serviceURL("NOTIFICATIONS_POS_APP_URL", sharedPOSBase), "/")
	if custom := strings.TrimRight(ti.ServiceURLs["pos"], "/"); custom != "" {
		posBase = custom
	}
	manageLink := posBase + "/online-orders"
	if ti.Slug != "" {
		manageLink = posBase + "/" + ti.Slug + "/online-orders"
	}
	data := map[string]interface{}{
		"outlet_name":      evtData["outlet_name"],
		"order_number":     orderNumber,
		"order_id":         orderID,
		"customer_name":    customer,
		"total_amount":     total,
		"delivery_address": evtData["delivery_address"],
		"manage_link":      manageLink,
	}
	send := func(channel, to string, metadata map[string]interface{}) {
		msg := messaging.Message{
			TenantID:       tenantID,
			Channel:        channel,
			TemplateID:     "ordering/new_order_tenant",
			SenderScope:    messaging.SenderScopeTenant,
			Target:         messaging.TargetStaff,
			To:             []string{to},
			Data:           data,
			Metadata:       metadata,
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("new-order-tenant-%s-%s", orderID, channel),
			QueuedAt:       time.Now(),
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Warn("failed to dispatch business new-order alert", zap.String("order_id", orderID), zap.String("channel", channel), zap.Error(err))
		}
	}
	if ti.ContactEmail != "" {
		send("email", ti.ContactEmail, map[string]interface{}{
			"subject":    fmt.Sprintf("New order %s: accept it in the online orders queue", orderNumber),
			"service_id": "ordering",
		})
	}
	if ti.ContactPhone != "" {
		params := []string{waParam(orderNumber, "-"), customer, total, waParam(fulfilment, "Online order")}
		meta := map[string]interface{}{
			"service_id":        "ordering",
			"template_name":     "ordering_new_order_business_v1",
			"template_language": "en_US",
			"template_params":   append(append([]string{}, params...), manageLink),
		}
		// On the shared POS domain the link is an "Open Orders" button (the approved button URL
		// can only vary the part after the domain); the plain-link template is the fallback.
		if strings.HasPrefix(manageLink, sharedPOSBase+"/") {
			meta["template_name"] = "ordering_new_order_business_v1_btn"
			meta["template_params"] = params
			meta["template_button_param"] = strings.TrimPrefix(manageLink, sharedPOSBase+"/")
			meta["template_fallback_name"] = "ordering_new_order_business_v1"
			meta["template_fallback_params"] = append(append([]string{}, params...), manageLink)
		}
		if code := dialCodeForCountry(ti.Country); code != "" {
			meta["default_dial_code"] = code
		}
		send("whatsapp", ti.ContactPhone, meta)
	}
}
