package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// orderEvent is the CloudEvents envelope from ordering-service.
// Ordering-backend publishes CloudEvents with "type"/"tenantId"/"data" fields
// (not shared-events "event_type"/"aggregate_type"/"tenant_id"/"payload").
type orderEvent struct {
	ID            string                 `json:"id"`
	Type          string                 `json:"type"`
	TenantID      string                 `json:"tenantId"`
	Data          map[string]interface{} `json:"data"`
	// shared-events fallback fields (for forward compatibility)
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	FallbackTID   string                 `json:"tenant_id"`
	Payload       map[string]interface{} `json:"payload"`
}

// resolvedType returns the event type, preferring CloudEvents "type" over shared-events "event_type".
func (e *orderEvent) resolvedType() string {
	if e.Type != "" {
		return e.Type
	}
	if e.AggregateType != "" && e.EventType != "" {
		return e.AggregateType + "." + e.EventType
	}
	return e.EventType
}

// resolvedTenantID returns the tenant ID from whichever field is populated.
func (e *orderEvent) resolvedTenantID() string {
	if e.TenantID != "" {
		return e.TenantID
	}
	return e.FallbackTID
}

// resolvedData returns the event data from whichever field is populated.
func (e *orderEvent) resolvedData() map[string]interface{} {
	if e.Data != nil {
		return e.Data
	}
	return e.Payload
}

// orderNotificationMapping maps event types to notification details.
type orderNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(data map[string]interface{}, orderAppURL string) map[string]interface{}
	// IdempotencyScope, when non-empty, overrides the default per-event-type
	// idempotency key with "order-<scope>-<orderID>". Distinct event types that
	// should produce a single deduplicated email share the same scope. The review
	// email is sent on BOTH ordering.order.delivered (delivery orders terminate at
	// "delivered") and ordering.order.completed (pickup/dine-in terminate at
	// "completed"); since the state machine also allows delivered→completed, an order
	// can emit both events — the shared "review" scope ensures only one review email.
	IdempotencyScope string
}

// orderAppBaseURL returns the ordering app URL for building "View Order" links.
func orderAppBaseURL(tenantSlug, tenantWebsite string) string {
	return serviceURLWithSlug("NOTIFICATIONS_ORDERING_APP_URL", tenantSlug, tenantWebsite)
}

// orderLink builds the public "View Order" URL for customer emails. It points at the
// tenant-scoped guest order page ({app}/{slug}/orders/guest/{id}), which opens without
// a login for both guest and authenticated-user orders — the unguessable order UUID is
// the access capability. orderAppURL may or may not already carry the slug (per-tenant
// config), so the slug is only appended when missing to avoid a doubled segment.
func orderLink(data map[string]interface{}, orderAppURL string) string {
	orderID, _ := data["order_id"].(string)
	slug, _ := data["tenant_slug"].(string)
	base := strings.TrimRight(orderAppURL, "/")
	if slug != "" && !strings.HasSuffix(base, "/"+slug) {
		base = base + "/" + slug
	}
	return fmt.Sprintf("%s/orders/guest/%s", base, orderID)
}

// reviewEmailDataBuilder builds the data for the post-delivery review/rating email.
// Shared by both ordering.order.completed (pickup/dine-in) and ordering.order.delivered
// (delivery), so the "Leave a rating / review" link is identical regardless of which
// terminal event fires.
func reviewEmailDataBuilder(data map[string]interface{}, orderAppURL string) map[string]interface{} {
	return map[string]interface{}{
		"name":        data["customer_name"],
		"order_id":    data["order_id"],
		"order_link":  orderLink(data, orderAppURL),
		"review_link": orderLink(data, orderAppURL) + "?rate=1",
	}
}

var orderMappings = map[string]orderNotificationMapping{
	"ordering.order.created": {
		TemplateID:   "ordering/order_placed",
		EmailSubject: "Your order has been confirmed",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":                data["customer_name"],
				"order_id":            data["order_id"],
				"total_amount":        data["total_amount"],
				"estimated_prep_time": data["estimated_prep_time"],
				"delivery_address":    data["delivery_address"],
				"order_link":          orderLink(data, orderAppURL),
				// "Pay Now" deep-link to the public guest order page with the Pay-Now modal
				// auto-opened (?pay=1). The order.created event carries no payment-status flag,
				// so we always provide the link; if the order is already paid the guest page
				// simply shows the order without prompting for payment.
				"pay_link": orderLink(data, orderAppURL) + "?pay=1",
				// Proof-of-delivery code (6-digit); empty for non-delivery orders.
				// The template only renders the PoD block when this is non-empty.
				"pod_code": data["pod_code"],
			}
		},
	},
	"ordering.order.ready": {
		TemplateID:   "ordering/order_ready",
		EmailSubject: "Your order is ready",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":       data["customer_name"],
				"order_id":   data["order_id"],
				"order_link": orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.out_for_delivery": {
		TemplateID:   "ordering/order_out_for_delivery",
		EmailSubject: "Your order is out for delivery",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":        data["customer_name"],
				"order_id":    data["order_id"],
				"rider_name":  data["rider_name"],
				"rider_phone": data["rider_phone"],
				"order_link":  orderLink(data, orderAppURL),
				"track_link":  orderLink(data, orderAppURL),
			}
		},
	},
	// Pickup/dine-in orders terminate at "completed" and get the review/rating email.
	"ordering.order.completed": {
		TemplateID:       "ordering/order_delivered",
		EmailSubject:     "Your order has been delivered",
		DataBuilder:      reviewEmailDataBuilder,
		IdempotencyScope: "review",
	},
	// DELIVERY orders terminate at "delivered" (they never reach "completed"), so the
	// review/rating email must also fire here — using the same template, subject, and
	// builder as completed. The shared "review" idempotency scope prevents a second
	// review email if an order emits both delivered and completed (delivered→completed
	// is a permitted transition).
	"ordering.order.delivered": {
		TemplateID:       "ordering/order_delivered",
		EmailSubject:     "Your order has been delivered",
		DataBuilder:      reviewEmailDataBuilder,
		IdempotencyScope: "review",
	},
	"ordering.order.cancelled": {
		TemplateID:   "ordering/order_cancelled",
		EmailSubject: "Your order has been cancelled",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":          data["customer_name"],
				"order_id":      data["order_id"],
				"cancel_reason": data["cancel_reason"],
				"order_link":    orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.refunded": {
		TemplateID:   "ordering/order_refunded",
		EmailSubject: "Your refund has been processed",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_number": data["order_number"],
				"amount":       data["total_amount"],
				"currency":     data["currency"],
				"reason":       data["reason"],
				"order_link":   orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.scheduled": {
		TemplateID:   "ordering/order_scheduled",
		EmailSubject: "Your order has been scheduled",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":          data["customer_name"],
				"order_number":  data["order_number"],
				"scheduled_for": data["scheduled_for"],
				"total_amount":  data["total_amount"],
				"currency":      data["currency"],
				"order_link":    orderLink(data, orderAppURL),
			}
		},
	},
	"ordering.order.for_pickup": {
		TemplateID:   "ordering/order_for_pickup",
		EmailSubject: "Your order is ready for pickup",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_number": data["order_number"],
				"outlet_name":  data["outlet_name"],
				"pickup_time":  data["pickup_time"],
				"order_link":   orderLink(data, orderAppURL),
			}
		},
	},
}

// startOrderConsumer subscribes to ordering.order.> events and dispatches
// customer notifications for order status changes.
func startOrderConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping order consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt orderEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("order event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		evtType := evt.resolvedType()
		evtTenantID := evt.resolvedTenantID()
		evtData := evt.resolvedData()

		mapping, ok := orderMappings[evtType]
		if !ok {
			logg.Debug("order event: unhandled type, skipping", zap.String("type", evtType))
			_ = m.Ack()
			return
		}

		// Extract customer email from event data
		email, _ := evtData["customer_email"].(string)
		if email == "" {
			logg.Warn("order event: no customer_email in data, skipping", zap.String("type", evtType))
			_ = m.Ack()
			return
		}

		// Resolve tenant info for building order links
		var ti *tenantInfo
		tenantWebsite := ""
		tenantSlug := ""
		if resolved, err := tr.resolve(ctx, evtTenantID); err == nil {
			ti = resolved
			tenantWebsite = ti.Website
			tenantSlug = ti.Slug
		} else {
			logg.Warn("order event: could not resolve tenant, using empty website", zap.String("tenant_id", evtTenantID), zap.Error(err))
		}

		// Use per-tenant ordering app URL when available, otherwise fall back to env/website.
		appURL := ti.ServiceURL("ordering", "NOTIFICATIONS_ORDERING_APP_URL", orderAppBaseURL(tenantSlug, tenantWebsite))

		orderID, _ := evtData["order_id"].(string)

		// Build the idempotency key. By default it is per-event-type so distinct
		// notifications for the same order don't collide. Mappings that set an
		// IdempotencyScope (e.g. the review email, emitted on BOTH delivered and
		// completed) share a single key so the email is sent at most once per order.
		idempotencyKey := fmt.Sprintf("order-%s-%s", evtType, orderID)
		if mapping.IdempotencyScope != "" {
			idempotencyKey = fmt.Sprintf("order-%s-%s", mapping.IdempotencyScope, orderID)
		}

		// Expose the tenant slug to DataBuilders so order/review links can target the
		// tenant-scoped public guest order page ({app}/{slug}/orders/guest/{id}).
		if tenantSlug != "" {
			evtData["tenant_slug"] = tenantSlug
		}

		msg := messaging.Message{
			TenantID:    evtTenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetCustomer,
			To:          []string{email},
			Data:        mapping.DataBuilder(evtData, appURL),
			Metadata: map[string]interface{}{
				"subject":    mapping.EmailSubject,
				"service_id": "ordering",
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: idempotencyKey,
			QueuedAt:       time.Now(),
		}

		// On a brand-new online order, send the tenant/outlet a dedicated, actionable
		// "new order arrived" alert — a SEPARATE email to the tenant contact address, using
		// its own staff-facing template. This is NOT a Bcc of the customer's confirmation
		// (which staff have no use for) and is never sent to the customer.
		if evtType == "ordering.order.created" && ti != nil && ti.ContactEmail != "" {
			tenantMsg := messaging.Message{
				TenantID:    evtTenantID,
				Channel:     "email",
				TemplateID:  "ordering/new_order_tenant",
				SenderScope: messaging.SenderScopeTenant,
				Target:      messaging.TargetStaff,
				To:          []string{ti.ContactEmail},
				Data: map[string]interface{}{
					"outlet_name":      evtData["outlet_name"],
					"order_number":     evtData["order_number"],
					"order_id":         evtData["order_id"],
					"customer_name":    evtData["customer_name"],
					"total_amount":     evtData["total_amount"],
					"delivery_address": evtData["delivery_address"],
					"manage_link":      serviceURL("NOTIFICATIONS_POS_APP_URL", tenantWebsite) + "/online-orders",
				},
				Metadata: map[string]interface{}{
					"subject":    "New order received — action required",
					"service_id": "ordering",
				},
				RequestID:      uuid.New().String(),
				IdempotencyKey: fmt.Sprintf("new-order-tenant-%s", orderID),
				QueuedAt:       time.Now(),
			}
			if _, terr := messaging.Publish(ctx, nc, cfg.Events, tenantMsg); terr != nil {
				logg.Warn("failed to dispatch tenant new-order alert", zap.String("order_id", orderID), zap.Error(terr))
			} else {
				logg.Info("tenant new-order alert dispatched", zap.String("order_id", orderID), zap.String("to", ti.ContactEmail))
			}
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("order event: failed to dispatch notification",
				zap.String("type", evtType),
				zap.String("order_id", orderID),
				zap.Error(err),
			)
			_ = m.Nak()
			return
		}

		logg.Info("order notification dispatched",
			zap.String("type", evtType),
			zap.String("template", mapping.TemplateID),
			zap.String("order_id", orderID),
			zap.String("to", email),
		)
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "ordering", "ordering.order.>", "notifications-ordering-status", handler,
		nats.BindStream("ordering"),
		nats.Durable("notifications-ordering-status"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
