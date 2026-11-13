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

// ticketingEvent is the shared-events envelope from ticketing-api.
type ticketingEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

// ticketAudience says who a ticket email is for.
type ticketAudience int

const (
	// audienceAgent: the assigned agent (agent_email), else the tenant's support contact.
	audienceAgent ticketAudience = iota
	// audienceRequester: the person who raised the ticket (requester_email, or customer_email on
	// events from before 13 November 2026). Never the tenant's mailbox: with no requester email
	// the message is not sent.
	audienceRequester
)

// ticketLinks are the two places a ticket email points to: the agent console for staff, and the
// public support page for the requester (the requester's own ticket view needs a token the event
// does not carry, so the requester is sent to the support page and can answer by replying).
type ticketLinks struct {
	Console string // ticket console base, e.g. https://ticketing.codevertexafrica.com/<org>
	Support string // public support page
}

type ticketingNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	Audience     ticketAudience
	// Skip, when set, drops an event this mapping should not send (a status the requester does
	// not need to hear about, a comment that is not an agent reply).
	Skip        func(payload map[string]any) bool
	DataBuilder func(payload map[string]any, links ticketLinks) map[string]any
}

// ticketText reads a string payload key.
func ticketText(p map[string]any, key string) string {
	s, _ := p[key].(string)
	return strings.TrimSpace(s)
}

// requesterEmail is the requester's address, from requester_email or the legacy customer_email.
func requesterEmail(p map[string]any) string {
	if e := ticketText(p, "requester_email"); e != "" {
		return e
	}
	return ticketText(p, "customer_email")
}

// requesterGreeting is the requester's name, or "there" ("Hello there,").
func requesterGreeting(p map[string]any) string {
	for _, k := range []string{"requester_name", "customer_name"} {
		if n := ticketText(p, k); n != "" {
			return n
		}
	}
	return "there"
}

// ticketStatusLabel is a status in words.
func ticketStatusLabel(s string) string {
	switch s {
	case "new":
		return "New"
	case "open":
		return "Open"
	case "in_progress":
		return "In progress"
	case "pending_customer":
		return "Waiting for your reply"
	case "pending_internal":
		return "Waiting on our team"
	case "resolved":
		return "Resolved"
	case "closed":
		return "Closed"
	case "cancelled":
		return "Cancelled"
	}
	return strings.ReplaceAll(s, "_", " ")
}

// requesterStatusNotice is what a status change tells the requester, or "" when the change is
// internal (new, open, in progress, waiting on the team) and not worth an email. Resolved has its
// own event (ticket.resolved).
func requesterStatusNotice(oldStatus, newStatus string) string {
	switch newStatus {
	case "pending_customer":
		return "We need a reply from you to continue. Reply to this email with the details we asked for."
	case "closed":
		return "This ticket is now closed. If you still need help, reply to this email."
	case "cancelled":
		return "This ticket has been cancelled. If that is a mistake, reply to this email."
	case "open", "in_progress":
		if oldStatus == "resolved" || oldStatus == "closed" {
			return "This ticket has been reopened and our team is looking at it again."
		}
	}
	return ""
}

func requesterBase(p map[string]any, links ticketLinks) map[string]any {
	return map[string]any{
		"name":        requesterGreeting(p),
		"ticket_id":   ticketText(p, "ticket_number"),
		"subject":     ticketText(p, "subject"),
		"status":      ticketStatusLabel(ticketText(p, "status")),
		"action_link": links.Support,
	}
}

var ticketingMappings = map[string]ticketingNotificationMapping{
	"ticket.created": {
		TemplateID:   "ticketing/ticket_created",
		EmailSubject: "We have received your support request",
		Audience:     audienceRequester,
		DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
			d := requesterBase(p, links)
			d["description"] = ticketText(p, "description")
			return d
		},
	},
	"ticket.comment_added": {
		TemplateID:   "ticketing/ticket_reply",
		EmailSubject: "New reply on your support ticket",
		Audience:     audienceRequester,
		// ticketing-api publishes public agent replies only; anything else is dropped here too.
		Skip: func(p map[string]any) bool {
			at := ticketText(p, "author_type")
			return (at != "" && at != "agent") || ticketText(p, "comment") == ""
		},
		DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
			d := requesterBase(p, links)
			d["comment"] = ticketText(p, "comment")
			return d
		},
	},
	"ticket.status_changed": {
		TemplateID:   "ticketing/ticket_status_changed",
		EmailSubject: "Update on your support ticket",
		Audience:     audienceRequester,
		Skip: func(p map[string]any) bool {
			return requesterStatusNotice(ticketText(p, "old_status"), ticketText(p, "new_status")) == ""
		},
		DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
			d := requesterBase(p, links)
			d["status"] = ticketStatusLabel(ticketText(p, "new_status"))
			d["status_notice"] = requesterStatusNotice(ticketText(p, "old_status"), ticketText(p, "new_status"))
			return d
		},
	},
	"ticket.resolved": {
		TemplateID:   "ticketing/ticket_resolved",
		EmailSubject: "Your ticket has been resolved",
		Audience:     audienceRequester,
		DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
			d := requesterBase(p, links)
			d["resolution_summary"] = ticketText(p, "resolution_summary")
			d["resolved_by"] = ticketText(p, "resolved_by")
			return d
		},
	},
	"ticket.assigned": {
		TemplateID:   "ticketing/ticket_assigned",
		EmailSubject: "Ticket assigned to you",
		Audience:     audienceAgent,
		DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
			customer := ticketText(p, "requester_name")
			if customer == "" {
				customer = ticketText(p, "customer_name")
			}
			return map[string]any{
				"name":          "Agent",
				"ticket_id":     p["ticket_number"],
				"subject":       p["subject"],
				"priority":      p["priority"],
				"customer_name": customer,
				"action_link":   fmt.Sprintf("%s/tickets/%s", links.Console, p["ticket_id"]),
			}
		},
	},
	// SLA breaches from ticketing-api's SLA worker (ticketing.sla.*), sent to the assigned agent.
	"sla.first_response_breached": ticketSLABreachMapping("First response"),
	"sla.resolution_breached":     ticketSLABreachMapping("Resolution"),
}

// ticketSLABreachMapping is the agent alert for one kind of SLA breach.
func ticketSLABreachMapping(label string) ticketingNotificationMapping {
	return ticketingNotificationMapping{
		TemplateID:   "ticketing/ticket_sla_breached",
		EmailSubject: label + " SLA breached on a support ticket",
		Audience:     audienceAgent,
		DataBuilder: func(payload map[string]any, links ticketLinks) map[string]any {
			name := "Agent"
			if n, ok := payload["agent_name"].(string); ok && n != "" {
				name = n
			}
			return map[string]any{
				"name":         name,
				"breach_label": label,
				"ticket_id":    payload["ticket_number"],
				"subject":      payload["subject"],
				"priority":     payload["priority"],
				"action_link":  fmt.Sprintf("%s/tickets/%s", links.Console, payload["ticket_id"]),
			}
		},
	}
}

// platformSupportEmail answers the platform's own tickets (the codevertex tenant) when its
// support contact is not set (support routing, owner decisions 12 November 2026).
const platformSupportEmail = "support@codevertexafrica.com"

// ticketSupportEmail is the tenant's support address: the Reply-To on requester mail and the
// recipient of agent alerts on an unassigned ticket.
func ticketSupportEmail(ti *tenantInfo) string {
	if ti.SupportEmail != "" {
		return ti.SupportEmail
	}
	if ti.ContactEmail != "" {
		return ti.ContactEmail
	}
	if ti.Slug == "codevertex" {
		return platformSupportEmail
	}
	return ""
}

// ticketRoute picks the recipient, target and Reply-To for a mapped event; ok false means the
// event has nobody to send to.
func ticketRoute(m ticketingNotificationMapping, payload map[string]any, ti *tenantInfo) (to, target, replyTo string, ok bool) {
	support := ticketSupportEmail(ti)
	if m.Audience == audienceRequester {
		to = requesterEmail(payload)
		return to, messaging.TargetCustomer, support, to != ""
	}
	// Staff alerts go to the assigned agent; the tenant's support contact covers an unassigned ticket.
	to = ticketText(payload, "agent_email")
	if to == "" {
		to = support
	}
	return to, messaging.TargetStaff, "", to != ""
}

// ticketIdempotencyKey is one per event: a ticket has many comments and status changes, so the
// ticket id alone would swallow every one after the first.
func ticketIdempotencyKey(evt ticketingEvent) string {
	id := evt.ID
	if id == "" {
		id = evt.AggregateID + "-" + evt.Timestamp
	}
	return fmt.Sprintf("ticketing-%s-%s", evt.EventType, id)
}

// startTicketingConsumer subscribes to ticketing.> events and dispatches ticket emails: the
// requester hears about their ticket (received, agent replies, status changes that need them,
// resolved) with the tenant's support address as Reply-To, and agents get assignments and SLA
// breaches.
func startTicketingConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping ticketing consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt ticketingEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("ticketing event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		mapping, ok := ticketingMappings[evt.EventType]
		if !ok {
			logg.Debug("ticketing event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}
		if mapping.Skip != nil && mapping.Skip(evt.Payload) {
			_ = m.Ack()
			return
		}

		tenantID := evt.TenantID
		if tenantID == "" {
			logg.Warn("ticketing event: no tenant_id, skipping")
			_ = m.Ack()
			return
		}

		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			logg.Error("ticketing event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}

		to, target, replyTo, ok := ticketRoute(mapping, evt.Payload, ti)
		if !ok {
			logg.Info("ticketing event: no recipient, skipping",
				zap.String("type", evt.EventType), zap.String("tenant", ti.Slug))
			_ = m.Ack()
			return
		}

		links := ticketLinks{
			Console: ti.Website,
			Support: ti.ServiceURL("ticketing", "NOTIFICATIONS_TICKETING_APP_URL", "https://ticketing.codevertexafrica.com") + "/support",
		}
		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      target,
			To:          []string{to},
			ReplyTo:     replyTo,
			Data:        mapping.DataBuilder(evt.Payload, links),
			Metadata: map[string]any{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: ticketIdempotencyKey(evt),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("ticketing event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}

		logg.Info("ticketing notification dispatched", zap.String("type", evt.EventType), zap.String("template", mapping.TemplateID))
		_ = m.Ack()
	}

	eventslib.SubscribeQueueWithRebind(logg, js, "ticketing", "ticketing.>", "notifications-ticketing", handler,
		nats.BindStream("ticketing"),
		nats.Durable("notifications-ticketing"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
