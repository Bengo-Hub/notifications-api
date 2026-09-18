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
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// orderEvent is the CloudEvents envelope from ordering-service.
// Ordering-backend publishes CloudEvents with "type"/"tenantId"/"data" fields
// (not shared-events "event_type"/"aggregate_type"/"tenant_id"/"payload").
type orderEvent struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	TenantID string                 `json:"tenantId"`
	Data     map[string]interface{} `json:"data"`
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
	// WhatsAppTemplate, when non-empty, is the Meta-approved template name (see
	// internal/whatsapp/templatesync/templates.json) used for this notification's WhatsApp
	// send instead of the freeform text render. Meta only allows freeform business-initiated
	// text within an active 24h customer-service window; nearly every order notification is
	// sent outside that window, so without this every WhatsApp send here silently required the
	// customer to have messaged first. Left empty, the send falls back to freeform text (still
	// correct within an open window).
	WhatsAppTemplate string
	// WhatsAppParams builds the ordered {{1}}, {{2}}, ... positional values for WhatsAppTemplate
	// from the same msgData map DataBuilder already produced, in the exact order Meta's approved
	// template body expects them (see templates.json's "body"/"example" for each template name).
	// For a mapping with WhatsAppButtonTemplate set, this MUST end with the link value named by
	// WhatsAppLinkKey as its last element — the button path reuses this slice minus that last
	// element as the button variant's (shorter) body params.
	WhatsAppParams func(msgData map[string]interface{}) []string
	// WhatsAppButtonTemplate, when non-empty, is a second Meta-approved template — identical to
	// WhatsAppTemplate but with the link moved out of the body text into a tappable URL button —
	// used instead of WhatsAppTemplate for tenants on the shared ordering domain (see
	// isSharedOrderingDomain). Left empty for mappings with no link (e.g. order_refunded) or
	// where a button doesn't make sense.
	WhatsAppButtonTemplate string
	// WhatsAppLinkKey names the msgData key (e.g. "order_link", "review_link") holding the full
	// URL that becomes the button's dynamic suffix when WhatsAppButtonTemplate is used.
	WhatsAppLinkKey string
	// WhatsAppOriginalTemplate/WhatsAppOriginalParams are the ALREADY Meta-approved template +
	// its (older, name-less) param order this mapping shipped with before the polished "_v3"/
	// "_v3_btn" rewrite. Every send here tries the new, polished primary first and falls back to
	// this proven-working template on any failure (see MetaCloudProvider.SendWhatsApp) — so a
	// customer keeps receiving correctly formatted messages the whole time Meta is reviewing the
	// new templates, never a hard failure.
	//
	// NOTE (2026-09-18): the original 8 templates were deleted from Meta along with the first
	// "_v2"/"_btn" draft (see project plan log #24) — Meta enforces a real cooldown before a
	// deleted template's exact name+language can be recreated ("Message template language is
	// being deleted... try again in 4 weeks"), so this fallback is itself temporarily
	// non-functional until that clears. No code change needed when it does: Run() will simply
	// succeed recreating these names again on its own schedule.
	WhatsAppOriginalTemplate string
	WhatsAppOriginalParams   func(msgData map[string]interface{}) []string
}

// sharedOrderingButtonPrefix is the ONE fixed domain Meta bakes into every "_btn" template's URL
// button at approval time (see templates.json) — Meta allows a button URL to vary only a suffix
// after a fixed prefix, never the domain itself, so button templates are only valid for tenants
// actually served from this domain. Must match NOTIFICATIONS_ORDERING_APP_URL's real value
// exactly (verified live: https://ordering.codevertexafrica.com).
const sharedOrderingButtonPrefix = "https://ordering.codevertexafrica.com/"

// isSharedOrderingDomain reports whether ti resolves its ordering links to the shared domain the
// "_btn" templates were approved against, rather than a tenant-specific custom domain (e.g.
// kuraweigh.kura.go.ke) — ServiceURLs["ordering"] is only set when a tenant has such an override.
func isSharedOrderingDomain(ti *tenantInfo) bool {
	return ti == nil || ti.ServiceURLs["ordering"] == ""
}

// buttonURLSuffix strips sharedOrderingButtonPrefix from a full order/review link, returning the
// dynamic suffix a "_btn" template's URL button parameter expects. Returns "" (meaning: don't use
// the button template) if fullURL doesn't actually start with that fixed prefix — a defensive
// check, since isSharedOrderingDomain already gates this, but a mismatched value here would
// otherwise silently send a broken button link.
func buttonURLSuffix(fullURL string) string {
	if !strings.HasPrefix(fullURL, sharedOrderingButtonPrefix) {
		return ""
	}
	return strings.TrimPrefix(fullURL, sharedOrderingButtonPrefix)
}

// waParam coerces a DataBuilder-produced value into a display string for a WhatsApp template
// positional parameter, falling back to def when empty/nil — Meta rejects an empty-string
// parameter, so every position must resolve to something visible.
func waParam(v interface{}, def string) string {
	s := fmt.Sprintf("%v", v)
	if v == nil || s == "" || s == "<nil>" {
		return def
	}
	return s
}

// formatMoney renders a raw amount + ISO currency code as "KES 2,450" (thousands-separated,
// no decimals when whole, 2 decimals otherwise) — matching the format Meta's approved order
// templates were drafted and approved against (see templates.json's "example" values). Amounts
// arrive as float64 off the wire (encoding/json decodes all JSON numbers that way).
func formatMoney(amount interface{}, currency interface{}) string {
	amt, _ := amount.(float64)
	cur, _ := currency.(string)
	if cur == "" {
		cur = "KES"
	}
	whole := int64(amt)
	frac := amt - float64(whole)
	// Thousands-group the integer part.
	digits := fmt.Sprintf("%d", whole)
	neg := strings.HasPrefix(digits, "-")
	if neg {
		digits = digits[1:]
	}
	var grouped strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(d)
	}
	numStr := grouped.String()
	if neg {
		numStr = "-" + numStr
	}
	if frac < -0.005 || frac > 0.005 {
		numStr = fmt.Sprintf("%s.%02d", numStr, int64(frac*100+0.5))
	}
	return cur + " " + numStr
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
		"name": data["customer_name"],
		// order.completed (pickup/dine-in) carries no delivered_at; the delivered_at line in
		// order_delivered.html is guarded with {{ if }} specifically so this is fine when empty.
		"delivered_at": data["delivered_at"],
		"order_number": data["order_number"],
		"order_id":     data["order_id"],
		"order_link":   orderLink(data, orderAppURL),
		"review_link":  orderLink(data, orderAppURL) + "?rate=1",
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
				"order_number":        data["order_number"],
				"total_amount":        formatMoney(data["total_amount"], data["currency"]),
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
		WhatsAppTemplate:       "ordering_order_placed_v3",
		WhatsAppButtonTemplate: "ordering_order_placed_v3_btn",
		WhatsAppLinkKey:        "order_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			est := ""
			if v, ok := d["estimated_prep_time"]; ok && v != nil {
				if s := fmt.Sprintf("%v", v); s != "" && s != "0" {
					est = s + " min"
				}
			}
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["total_amount"], "-"),
				waParam(est, "As soon as possible"),
				waParam(d["order_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_placed",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			est := ""
			if v, ok := d["estimated_prep_time"]; ok && v != nil {
				if s := fmt.Sprintf("%v", v); s != "" && s != "0" {
					est = s + " min"
				}
			}
			return []string{
				waParam(d["order_number"], "your order"),
				waParam(d["total_amount"], "-"),
				waParam(est, "As soon as possible"),
				waParam(d["order_link"], ""),
			}
		},
	},
	"ordering.order.ready": {
		TemplateID:   "ordering/order_ready",
		EmailSubject: "Your order is ready",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_id":     data["order_id"],
				"order_number": data["order_number"],
				"order_link":   orderLink(data, orderAppURL),
			}
		},
		WhatsAppTemplate:       "ordering_order_ready_v3",
		WhatsAppButtonTemplate: "ordering_order_ready_v3_btn",
		WhatsAppLinkKey:        "order_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["order_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_ready",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["order_link"], ""),
			}
		},
	},
	"ordering.order.out_for_delivery": {
		TemplateID:   "ordering/order_out_for_delivery",
		EmailSubject: "Your order is out for delivery",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_id":     data["order_id"],
				"order_number": data["order_number"],
				"rider_name":   data["rider_name"],
				"rider_phone":  data["rider_phone"],
				"order_link":   orderLink(data, orderAppURL),
				"track_link":   orderLink(data, orderAppURL),
			}
		},
		WhatsAppTemplate:       "ordering_order_out_for_delivery_v3",
		WhatsAppButtonTemplate: "ordering_order_out_for_delivery_v3_btn",
		WhatsAppLinkKey:        "track_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["rider_name"], "your rider"),
				waParam(d["rider_phone"], "-"),
				waParam(d["track_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_out_for_delivery",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["rider_name"], "your rider"),
				waParam(d["rider_phone"], "-"),
				waParam(d["track_link"], ""),
			}
		},
	},
	// Pickup/dine-in orders terminate at "completed" and get the review/rating email.
	"ordering.order.completed": {
		TemplateID:             "ordering/order_delivered",
		EmailSubject:           "Your order has been delivered",
		DataBuilder:            reviewEmailDataBuilder,
		IdempotencyScope:       "review",
		WhatsAppTemplate:       "ordering_order_delivered_v3",
		WhatsAppButtonTemplate: "ordering_order_delivered_v3_btn",
		WhatsAppLinkKey:        "review_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["review_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_delivered",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["order_number"], "your order"),
				waParam(d["review_link"], ""),
			}
		},
	},
	// DELIVERY orders terminate at "delivered" (they never reach "completed"), so the
	// review/rating email must also fire here — using the same template, subject, and
	// builder as completed. The shared "review" idempotency scope prevents a second
	// review email if an order emits both delivered and completed (delivered→completed
	// is a permitted transition).
	"ordering.order.delivered": {
		TemplateID:             "ordering/order_delivered",
		EmailSubject:           "Your order has been delivered",
		DataBuilder:            reviewEmailDataBuilder,
		IdempotencyScope:       "review",
		WhatsAppTemplate:       "ordering_order_delivered_v3",
		WhatsAppButtonTemplate: "ordering_order_delivered_v3_btn",
		WhatsAppLinkKey:        "review_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["review_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_delivered",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["order_number"], "your order"),
				waParam(d["review_link"], ""),
			}
		},
	},
	"ordering.order.cancelled": {
		TemplateID:   "ordering/order_cancelled",
		EmailSubject: "Your order has been cancelled",
		DataBuilder: func(data map[string]interface{}, orderAppURL string) map[string]interface{} {
			return map[string]interface{}{
				"name":         data["customer_name"],
				"order_id":     data["order_id"],
				"order_number": data["order_number"],
				// The event's own field is "reason" (see OrderCancelledData in
				// ordering-backend/internal/platform/events/publisher.go) — this previously read
				// "cancel_reason", a key that was never actually present in the event payload, so
				// the cancellation reason silently never appeared in any cancellation notification.
				"cancel_reason": data["reason"],
				"order_link":    orderLink(data, orderAppURL),
			}
		},
		WhatsAppTemplate:       "ordering_order_cancelled_v3",
		WhatsAppButtonTemplate: "ordering_order_cancelled_v3_btn",
		WhatsAppLinkKey:        "order_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["cancel_reason"], "Not specified"),
				waParam(d["order_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_cancelled",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["cancel_reason"], "Not specified"),
				waParam(d["order_link"], ""),
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
				"amount":       formatMoney(data["total_amount"], data["currency"]),
				"currency":     data["currency"],
				"reason":       data["reason"],
				"order_link":   orderLink(data, orderAppURL),
			}
		},
		WhatsAppTemplate: "ordering_order_refunded_v3",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["amount"], "-"),
				waParam(d["order_number"], "your order"),
				waParam(d["reason"], "Not specified"),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_refunded",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["amount"], "-"),
				waParam(d["order_number"], "your order"),
				waParam(d["reason"], "Not specified"),
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
				"total_amount":  formatMoney(data["total_amount"], data["currency"]),
				"currency":      data["currency"],
				"order_link":    orderLink(data, orderAppURL),
			}
		},
		WhatsAppTemplate:       "ordering_order_scheduled_v3",
		WhatsAppButtonTemplate: "ordering_order_scheduled_v3_btn",
		WhatsAppLinkKey:        "order_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["scheduled_for"], "the scheduled time"),
				waParam(d["total_amount"], "-"),
				waParam(d["order_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_scheduled",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["scheduled_for"], "the scheduled time"),
				waParam(d["total_amount"], "-"),
				waParam(d["order_link"], ""),
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
		WhatsAppTemplate:       "ordering_order_for_pickup_v3",
		WhatsAppButtonTemplate: "ordering_order_for_pickup_v3_btn",
		WhatsAppLinkKey:        "order_link",
		WhatsAppParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["outlet_name"], "our store"),
				waParam(d["pickup_time"], "shortly"),
				waParam(d["order_link"], ""),
			}
		},
		WhatsAppOriginalTemplate: "ordering_order_for_pickup",
		WhatsAppOriginalParams: func(d map[string]interface{}) []string {
			return []string{
				waParam(d["name"], "there"),
				waParam(d["order_number"], "your order"),
				waParam(d["outlet_name"], "our store"),
				waParam(d["pickup_time"], "shortly"),
				waParam(d["order_link"], ""),
			}
		},
	},
}

// startOrderConsumer subscribes to ordering.order.> events and dispatches
// customer notifications for order status changes.
func startOrderConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, gate *preferences.Gate, templateChannels map[string][]string, logg *zap.Logger) {
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

		// Extract customer contact info from event data — ordering-service's publisher
		// includes customer_phone on every order event alongside customer_email (verified
		// against internal/platform/events/publisher.go), so SMS/WhatsApp fan-out has a real
		// recipient to use, not a guessed field.
		email, _ := evtData["customer_email"].(string)
		phone, _ := evtData["customer_phone"].(string)
		if email == "" && phone == "" {
			logg.Warn("order event: no customer contact info in data, skipping", zap.String("type", evtType))
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

		// Fan out to every channel the tenant has enabled for this notification type that
		// also has a drafted template and a recipient contact — not just email. A tenant that
		// only enabled WhatsApp for order updates gets only WhatsApp; one that enabled both
		// gets both. See fanOutTargets/publishFanOut (cmd/worker/fanout.go).
		recipients := map[string]string{}
		if email != "" {
			recipients["email"] = email
		}
		if phone != "" {
			recipients["sms"] = phone
			recipients["whatsapp"] = phone
		}
		msgData := mapping.DataBuilder(evtData, appURL)
		metadata := map[string]interface{}{
			"subject":    mapping.EmailSubject,
			"service_id": "ordering",
		}
		// Attaching template_name/template_params here only changes the WhatsApp send path
		// (deliver()'s "whatsapp" case, internal/providers/whatsapp/metacloud.go's SendWhatsApp) —
		// email/SMS/push ignore these metadata keys and keep rendering the local template as
		// before. Meta requires a pre-approved template for any business-initiated message sent
		// outside an active 24h customer-service reply window, which is true for nearly every
		// order notification; without this, WhatsApp sends here only worked when the customer had
		// messaged the business first.
		if mapping.WhatsAppTemplate != "" && mapping.WhatsAppParams != nil {
			params := mapping.WhatsAppParams(msgData)
			// Prefer the button variant when one exists for this mapping AND this tenant resolves
			// ordering links to the shared domain the button was approved against (see
			// isSharedOrderingDomain/buttonURLSuffix) — a tenant on a custom domain keeps the
			// plain-text-link template instead, since Meta can't vary the button's domain per send.
			if mapping.WhatsAppButtonTemplate != "" && mapping.WhatsAppLinkKey != "" && isSharedOrderingDomain(ti) && len(params) > 0 {
				linkVal := fmt.Sprintf("%v", msgData[mapping.WhatsAppLinkKey])
				if suffix := buttonURLSuffix(linkVal); suffix != "" {
					metadata["template_name"] = mapping.WhatsAppButtonTemplate
					metadata["template_language"] = "en_US"
					metadata["template_params"] = params[:len(params)-1] // drop the link — it's the button now
					metadata["template_button_param"] = suffix
				}
			}
			if metadata["template_name"] == nil {
				metadata["template_name"] = mapping.WhatsAppTemplate
				metadata["template_language"] = "en_US"
				metadata["template_params"] = params
			}
			// Both the button and the polished-plain "_v2" template are new, freshly drafted
			// (see templatesync/templates.json) and need their own one-time Meta sync/approval
			// before they actually exist on the WABA — this code ships ahead of that. Point the
			// fallback at WhatsAppOriginalTemplate, the template this mapping shipped with
			// before this rewrite and which is ALREADY approved and live — so a pending/rejected
			// new template degrades straight to today's known-working send (old wording, no
			// button) instead of failing outright. See SendWhatsApp's retry-on-any-failure logic.
			if mapping.WhatsAppOriginalTemplate != "" && mapping.WhatsAppOriginalParams != nil {
				metadata["template_fallback_name"] = mapping.WhatsAppOriginalTemplate
				metadata["template_fallback_params"] = mapping.WhatsAppOriginalParams(msgData)
			}
		}
		base := messaging.Message{
			TenantID:       evtTenantID,
			TemplateID:     mapping.TemplateID,
			SenderScope:    messaging.SenderScopeTenant,
			Target:         messaging.TargetCustomer,
			Data:           msgData,
			Metadata:       metadata,
			IdempotencyKey: idempotencyKey,
		}
		targets := fanOutTargets(ctx, gate, templateChannels, evtTenantID, mapping.TemplateID, recipients)

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

		sentChannels := publishFanOut(ctx, nc, cfg, base, targets, logg)
		if len(sentChannels) == 0 {
			logg.Warn("order event: no channel dispatched (none enabled/templated for this tenant+type)",
				zap.String("type", evtType),
				zap.String("order_id", orderID),
			)
			_ = m.Ack()
			return
		}

		logg.Info("order notification dispatched",
			zap.String("type", evtType),
			zap.String("template", mapping.TemplateID),
			zap.String("order_id", orderID),
			zap.Strings("channels", sentChannels),
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
