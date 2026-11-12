package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/contacts"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// deliveryEvent is the CloudEvents envelope from logistics-service task events.
type deliveryEvent struct {
	ID            string                 `json:"id"`
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	TenantID      string                 `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// deliveryNotificationMapping maps task event types to notification details.
type deliveryNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	// DataBuilder builds the template data from the event payload. trackingLink is the public
	// tracking page for the task ("" when there is no tracking code or app address).
	DataBuilder func(data map[string]interface{}, trackingLink string) map[string]interface{}
}

var deliveryMappings = map[string]deliveryNotificationMapping{
	"logistics.task.assigned": {
		TemplateID:   "logistics/delivery_assigned",
		EmailSubject: "Your delivery has been assigned",
		DataBuilder: func(data map[string]interface{}, trackingLink string) map[string]interface{} {
			return map[string]interface{}{
				"order_id": deliveryOrderRef(data),
				// The rider's name, never their fleet-member UUID (which this used to show). No
				// rider name, no Rider line (the template guards it).
				"driver_name":       firstNonEmpty(data["rider_name"]),
				"estimated_arrival": formatEventTime(data["estimated_arrival"]),
				"tracking_link":     trackingLink,
			}
		},
	},
	// The rider is on the way to the customer: en_route_dropoff on the per-leg flow, en_route on
	// the legacy one.
	"logistics.task.en_route_dropoff": deliveryEnRouteMapping,
	"logistics.task.en_route":         deliveryEnRouteMapping,
	"logistics.task.completed":        deliveryCompletedMapping,
	// Closed through the status endpoint (no proof of delivery record): logistics publishes
	// task.delivered instead of task.completed.
	"logistics.task.delivered": deliveryCompletedMapping,

	"logistics.task.failed": {
		TemplateID:   "logistics/delivery_failed",
		EmailSubject: "Delivery attempt failed",
		DataBuilder: func(data map[string]interface{}, _ string) map[string]interface{} {
			return map[string]interface{}{
				"order_id":       deliveryOrderRef(data),
				"failure_reason": firstNonEmpty(data["failure_reason"], data["reason"]),
			}
		},
	},
}

var deliveryEnRouteMapping = deliveryNotificationMapping{
	TemplateID:   "logistics/delivery_en_route",
	EmailSubject: "Your order is on its way",
	DataBuilder: func(data map[string]interface{}, trackingLink string) map[string]interface{} {
		return map[string]interface{}{
			"order_id":          deliveryOrderRef(data),
			"driver_name":       firstNonEmpty(data["rider_name"]),
			"driver_phone":      firstNonEmpty(data["rider_phone"]),
			"estimated_arrival": formatEventTime(data["estimated_arrival"]),
			"tracking_link":     trackingLink,
		}
	},
}

var deliveryCompletedMapping = deliveryNotificationMapping{
	TemplateID:   "logistics/delivery_completed",
	EmailSubject: "Your delivery has been completed",
	DataBuilder: func(data map[string]interface{}, trackingLink string) map[string]interface{} {
		return map[string]interface{}{
			"order_id":          deliveryOrderRef(data),
			"delivered_at":      formatEventTime(data["delivered_at"]),
			"proof_of_delivery": deliveryProof(data),
			"order_link":        trackingLink,
		}
	},
}

// deliveryOrderRef is what the customer knows the delivery by: the order number, else the
// task's tracking code.
func deliveryOrderRef(data map[string]interface{}) string {
	return firstNonEmpty(data["order_number"], data["tracking_code"])
}

// deliveryProof describes how the delivery was proven, from the task.completed keys logistics
// sends ("" when none applies). The photo itself stays private in logistics.
func deliveryProof(data map[string]interface{}) string {
	var parts []string
	switch via, _ := data["delivered_via"].(string); via {
	case "code":
		parts = append(parts, "Confirmed with your delivery code")
	case "override":
		parts = append(parts, "Confirmed by the business")
	}
	if r, _ := data["recipient_name"].(string); strings.TrimSpace(r) != "" {
		parts = append(parts, "Received by "+strings.TrimSpace(r))
	}
	if photo, _ := data["proof_photo"].(bool); photo {
		parts = append(parts, "Photo taken at the door")
	}
	return strings.Join(parts, ". ")
}

// deliveryTrackingLink is the public tracking page for a task (logistics-ui /track/{code}), or ""
// when the event has no tracking code.
func deliveryTrackingLink(data map[string]interface{}, ti *tenantInfo) string {
	code, _ := data["tracking_code"].(string)
	if strings.TrimSpace(code) == "" {
		return ""
	}
	base := ti.ServiceURL("logistics", "NOTIFICATIONS_LOGISTICS_APP_URL", logisticsAppButtonBase)
	if base == "" {
		return ""
	}
	return base + "/track/" + url.PathEscape(strings.TrimSpace(code))
}

// deliverySupportLink is a mailto link to the business's contact email ("" when it has none,
// and the template then shows no button). A phone is not turned into a link: html/template
// rewrites a tel: URL to "#ZgotmplZ", so the phone is shown as text (support_phone) instead.
func deliverySupportLink(ti *tenantInfo) string {
	if ti == nil {
		return ""
	}
	if e := strings.TrimSpace(ti.ContactEmail); e != "" {
		return "mailto:" + e
	}
	return ""
}

// deliveryContactID is the CRM contact the event names, or uuid.Nil.
func deliveryContactID(evt deliveryEvent) uuid.UUID {
	raw, _ := evt.Payload["crm_contact_id"].(string)
	return contacts.ParseID(raw)
}

// deliveryCustomer is the customer a task event is about. logistics-api sends crm_contact_id
// alone for a customer linked in the CRM, and customer_email, customer_name and customer_phone
// for an unlinked one; when both are present the CRM contact wins field by field. A linked
// customer with no email resolved is never mailed: noticeRetry when the CRM could not be
// reached, else noticeDrop. An unlinked customer without an email gives noticeSend with an empty
// Email, which the caller acks without sending.
func deliveryCustomer(ctx context.Context, res *contacts.Resolver, evt deliveryEvent, ti *tenantInfo) (contacts.Recipient, noticeOutcome) {
	email, _ := evt.Payload["customer_email"].(string)
	name, _ := evt.Payload["customer_name"].(string)
	phone, _ := evt.Payload["customer_phone"].(string)
	fallback := contacts.Recipient{Name: strings.TrimSpace(name), Email: strings.TrimSpace(email), Phone: strings.TrimSpace(phone)}
	id := deliveryContactID(evt)
	if id == uuid.Nil {
		return fallback, noticeSend
	}
	tenantID := contacts.ParseID(evt.TenantID)
	if ti != nil && ti.ID != uuid.Nil {
		tenantID = ti.ID
	}
	rec, err := res.Lookup(ctx, tenantID, id, fallback)
	if rec.Email == "" {
		if err != nil {
			return rec, noticeRetry
		}
		return rec, noticeDrop
	}
	return rec, noticeSend
}

// deliveryEmailData is the template data for a customer delivery email: the mapping's fields,
// the greeting and the business's support contact.
func deliveryEmailData(mapping deliveryNotificationMapping, payload map[string]interface{}, ti *tenantInfo, customer contacts.Recipient) map[string]interface{} {
	data := mapping.DataBuilder(payload, deliveryTrackingLink(payload, ti))
	data["name"] = firstNonEmpty(customer.Name, "there")
	if link := deliverySupportLink(ti); link != "" {
		data["support_link"] = link
	} else if ti != nil && strings.TrimSpace(ti.ContactPhone) != "" {
		data["support_phone"] = strings.TrimSpace(ti.ContactPhone)
	}
	return data
}

// isFirstDelivery reports whether m is delivered for the first time (JetStream NumDelivered of
// 1). When the metadata cannot be read the count is unknown and it reports true, so a rider is
// never left unalerted.
func isFirstDelivery(m interface {
	Metadata() (*nats.MsgMetadata, error)
}) bool {
	md, err := m.Metadata()
	if err != nil || md == nil {
		return true
	}
	return md.NumDelivered <= 1
}

// startDeliveryConsumer subscribes to logistics.task.> events and dispatches
// delivery notification emails. This is separate from the fleet consumer which
// handles logistics.fleet.> events for rider lifecycle notifications.
func startDeliveryConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, client *ent.Client, crmContacts *contacts.Resolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping delivery consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt deliveryEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("delivery event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		// A rider whose job was cancelled or taken back by a dispatcher is told at once
		// (logistics sets rider_user_id only when the rider did not do it themselves).
		if evt.EventType == "task.cancelled" || evt.EventType == "task.unassigned" {
			if riderUser, _ := evt.Payload["rider_user_id"].(string); riderUser != "" {
				if ti, err := tr.resolve(ctx, evt.TenantID); err == nil {
					notifyRiderJobRemoved(ctx, nc, cfg, client, ti, evt, logg)
				} else {
					logg.Warn("rider job removed: resolve tenant", zap.String("tenant_id", evt.TenantID), zap.Error(err))
				}
			}
		}

		mapping, ok := deliveryMappings[evt.AggregateType+"."+evt.EventType]
		if !ok {
			logg.Debug("delivery event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		ti, err := tr.resolve(ctx, evt.TenantID)
		if err != nil {
			logg.Error("delivery event: failed to resolve tenant", zap.String("tenant_id", evt.TenantID), zap.Error(err))
			_ = m.Nak()
			return
		}

		taskID, _ := evt.Payload["task_id"].(string)

		// The rider is told about a job assigned to them (push to their devices, and email), on
		// the first delivery only: a CRM retry below redelivers this event, and the rider must
		// not be alerted again.
		if evt.EventType == "task.assigned" && isFirstDelivery(m) {
			notifyRiderAssigned(ctx, nc, cfg, client, ti, evt, logg)
		}

		// Customer messages. Ordering owns the customer conversation for its orders (received,
		// accepted, on its way with rider and code, delivered; a failed delivery is a staff alert
		// there), so logistics sends none for them. These messages also used to fall back to the
		// tenant's contact email when the event had no customer, mailing the business "your
		// delivery has been assigned" for every task; they now go to the customer or to nobody.
		if source, _ := evt.Payload["source_service"].(string); source == "ordering" {
			_ = m.Ack()
			return
		}
		customer, outcome := deliveryCustomer(ctx, crmContacts, evt, ti)
		switch outcome {
		case noticeRetry:
			retryCRMLookup(m, logg, "delivery", evt.EventType, evt.TenantID, deliveryContactID(evt).String())
			return
		case noticeDrop:
			logg.Info("delivery event: linked customer has no email in the CRM, notice dropped",
				zap.String("type", evt.EventType), zap.String("tenant_id", evt.TenantID),
				zap.String("task_id", taskID), zap.String("crm_contact_id", deliveryContactID(evt).String()))
			_ = m.Ack()
			return
		}
		if customer.Email == "" {
			_ = m.Ack()
			return
		}

		msg := messaging.Message{
			TenantID:    evt.TenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetCustomer,
			To:          []string{customer.Email},
			Data:        deliveryEmailData(mapping, evt.Payload, ti, customer),
			Metadata: map[string]interface{}{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("delivery-%s-%s", evt.EventType, taskID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("delivery event: failed to dispatch notification",
				zap.String("type", evt.EventType),
				zap.String("task_id", taskID),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("delivery notification dispatched",
			zap.String("type", evt.EventType),
			zap.String("template", mapping.TemplateID),
			zap.String("task_id", taskID),
		)
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "logistics", "logistics.task.>", "notifications-logistics-delivery", handler,
		nats.BindStream("logistics"),
		nats.Durable("notifications-logistics-delivery"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		// Same count retryCRMLookup treats as the last delivery (crm_retry.go). Changing
		// crmRetryMaxDeliver needs this durable edited or deleted on the server first:
		// SubscribeQueueWithRebind gives up on a MaxDeliver mismatch and this consumer (rider
		// alerts included) stays down.
		nats.MaxDeliver(crmRetryMaxDeliver),
	)
}
