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

// orderDeliveryFailedEvent is ordering's alert that a delivery will not reach the customer as
// planned: the rider failed it after pickup, or the task was cancelled while the order is still
// open. It is meant for the business (notification.target "admin"), never the customer.
const orderDeliveryFailedEvent = "ordering.order.delivery_failed"

// deliveryFailedTemplate is the staff email. There is no Meta-approved WhatsApp template for this
// alert yet, so it goes by email only; logistics also raises its own dispatch-board alert.
const deliveryFailedTemplate = "ordering/delivery_failed_tenant"

// deliveryFailedAlertData builds the email data from the event payload. ordering may send the
// order id with its "order:" task reference prefix, which is stripped for display.
func deliveryFailedAlertData(evtData map[string]interface{}, manageLink string) map[string]interface{} {
	orderID := strings.TrimSpace(fmt.Sprintf("%v", evtData["order_id"]))
	if i := strings.LastIndex(orderID, ":"); i >= 0 {
		orderID = orderID[i+1:]
	}
	if orderID == "<nil>" {
		orderID = ""
	}
	reason := strings.TrimSpace(waParam(evtData["failure_reason"], ""))
	if reason == "" {
		reason = "No reason was given."
	}
	return map[string]interface{}{
		"outlet_name":    evtData["outlet_name"],
		"order_id":       orderID,
		"order_number":   waParam(evtData["order_number"], ""),
		"failure_reason": reason,
		"failed_at":      evtData["failed_at"],
		"manage_link":    manageLink,
	}
}

// deliveryFailedIdempotencyKey is one alert per delivery task. Without a task id it falls back to
// the order and the failure time, so a redelivered event still dedupes.
func deliveryFailedIdempotencyKey(evtData map[string]interface{}, orderID string) string {
	if task := strings.TrimSpace(waParam(evtData["task_id"], "")); task != "" {
		return "delivery-failed-tenant-" + task
	}
	return fmt.Sprintf("delivery-failed-tenant-%s-%s", orderID, waParam(evtData["failed_at"], ""))
}

// sendBusinessDeliveryFailedAlert emails the tenant contact that an order's delivery failed or was
// cancelled and needs a new rider or a call to the customer.
func sendBusinessDeliveryFailedAlert(ctx context.Context, nc *nats.Conn, cfg *config.Config, ti *tenantInfo, tenantID string, evtData map[string]interface{}, logg *zap.Logger) {
	if ti == nil || ti.ContactEmail == "" {
		return
	}
	data := deliveryFailedAlertData(evtData, posOnlineOrdersLink(ti))
	orderID, _ := data["order_id"].(string)
	label, _ := data["order_number"].(string)
	if label == "" {
		label = orderID
	}
	msg := messaging.Message{
		TenantID:    tenantID,
		Channel:     "email",
		TemplateID:  deliveryFailedTemplate,
		SenderScope: messaging.SenderScopeTenant,
		Target:      messaging.TargetStaff,
		To:          []string{ti.ContactEmail},
		Data:        data,
		Metadata: map[string]interface{}{
			"subject":    fmt.Sprintf("Delivery problem on order %s", label),
			"service_id": "ordering",
		},
		RequestID:      uuid.New().String(),
		IdempotencyKey: deliveryFailedIdempotencyKey(evtData, orderID),
		QueuedAt:       time.Now(),
	}
	if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
		logg.Warn("failed to dispatch delivery-failed alert", zap.String("order_id", orderID), zap.Error(err))
	}
}
