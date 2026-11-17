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
	"github.com/bengobox/notifications-api/internal/providers/email"
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
	// SubjectBuilder, when set, writes a subject from the payload (it names the ticket); an empty
	// result falls back to EmailSubject.
	SubjectBuilder func(payload map[string]any) string
	// ReplySubject, on requester mail, uses the event's email_reply_subject ("Re: <subject>
	// [TKT-000042]") so the email reads as a reply in the customer's own thread.
	ReplySubject bool
	// Route, when set, replaces ticketRoute for an alert that may go to several people (a team's
	// supervisors) or must not fall back to the tenant's support mailbox.
	Route func(payload map[string]any, ti *tenantInfo) (to []string, target, replyTo string, ok bool)
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
		ReplySubject: true,
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
	// Agent workspace and SLA follow-ups (ticketing_agent_alerts.go): a mention and an escalation
	// go to the agent or team concerned, a breach warning before the SLA runs out, and a merge
	// tells the requester which ticket now carries their request.
	"ticket.mentioned":   ticketMentionedMapping,
	"ticket.merged":      ticketMergedMapping,
	"sla.breach_warning": ticketBreachWarningMapping,
	"ticket.escalated":   ticketEscalatedMapping,
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

// ticketRecipients is a mapping's own Route when it has one, else ticketRoute.
func ticketRecipients(m ticketingNotificationMapping, payload map[string]any, ti *tenantInfo) (to []string, target, replyTo string, ok bool) {
	if m.Route != nil {
		return m.Route(payload, ti)
	}
	one, target, replyTo, ok := ticketRoute(m, payload, ti)
	if !ok {
		return nil, target, replyTo, false
	}
	return []string{one}, target, replyTo, true
}

// ticketEmailSubject is the mapping's subject for this payload.
func ticketEmailSubject(m ticketingNotificationMapping, payload map[string]any) string {
	if m.SubjectBuilder != nil {
		if s := m.SubjectBuilder(payload); s != "" {
			return s
		}
	}
	return m.EmailSubject
}

// Thread fields ticketing-api puts on ticket events, carried to the sender in msg.Metadata so a
// customer's reply to a ticket email comes back to the same ticket.
const (
	metaEmailThreadToken = "email_thread_token" // the ticket id, the local part of the thread's root Message-ID
	metaEmailInReplyTo   = "email_in_reply_to"  // "<id@domain>" of the customer's last message
	metaEmailReferences  = "email_references"   // the thread's Message-IDs, oldest first
)

// ticketRequestID is the message's request id, which the worker also uses as the local part of
// the email's Message-ID. ticketing-api names it on ticket.created (the ticket id) and on
// ticket.comment_added (the comment id) so it can recognise a reply to its own mail; a bounce is
// still matched through the same id. Other events get a fresh one.
func ticketRequestID(p map[string]any) string {
	if token := ticketText(p, "email_message_token"); email.IsMessageIDToken(token) {
		return token
	}
	return uuid.New().String()
}

// ticketSubjectLine is the subject for a mapped event. Requester mail carries the ticket tag
// ("[TKT-000042]") so a reply finds the ticket even when the client drops the thread headers, and
// an agent reply uses ticketing's "Re: <subject> [TKT-000042]" so it reads as part of the
// customer's own conversation. The subject is folded onto one line: it can hold the customer's
// own words, which must never start a new header.
func ticketSubjectLine(m ticketingNotificationMapping, p map[string]any) string {
	s := ticketEmailSubject(m, p)
	if m.Audience == audienceRequester {
		if r := ticketText(p, "email_reply_subject"); m.ReplySubject && r != "" {
			s = r
		} else if tag := ticketText(p, "email_subject_tag"); tag != "" && !strings.Contains(s, tag) {
			s += " " + tag
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

// ticketThreadMetadata copies the thread fields of a requester email into md. Staff alerts get
// none: an agent answering an alert by email must not land on the ticket as the customer.
func ticketThreadMetadata(md map[string]any, m ticketingNotificationMapping, p map[string]any) {
	if m.Audience != audienceRequester {
		return
	}
	if v := ticketText(p, metaEmailThreadToken); v != "" {
		md[metaEmailThreadToken] = v
	}
	if v := ticketText(p, metaEmailInReplyTo); v != "" {
		md[metaEmailInReplyTo] = v
	}
	if refs := metadataStrings(p[metaEmailReferences]); len(refs) > 0 {
		md[metaEmailReferences] = refs
	}
}

// metadataStrings reads a string list from an event or message field. Decoded JSON holds it as
// []any; a message built in process holds []string.
func metadataStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	}
	return nil
}

// withEmailThread puts a message's thread fields on the send context, for the In-Reply-To and
// References headers. A message without them is sent with neither header.
func withEmailThread(ctx context.Context, md map[string]any) context.Context {
	token, _ := md[metaEmailThreadToken].(string)
	inReplyTo, _ := md[metaEmailInReplyTo].(string)
	return email.WithThreadHeaders(ctx, token, inReplyTo, metadataStrings(md[metaEmailReferences]))
}

// ticketMessage is the email for a mapped ticketing event.
func ticketMessage(m ticketingNotificationMapping, evt ticketingEvent, to []string, target, replyTo string, links ticketLinks) messaging.Message {
	md := map[string]any{"subject": ticketSubjectLine(m, evt.Payload)}
	ticketThreadMetadata(md, m, evt.Payload)
	return messaging.Message{
		TenantID:       evt.TenantID,
		Channel:        "email",
		TemplateID:     m.TemplateID,
		SenderScope:    messaging.SenderScopeTenant,
		Target:         target,
		To:             to,
		ReplyTo:        replyTo,
		Data:           m.DataBuilder(evt.Payload, links),
		Metadata:       md,
		RequestID:      ticketRequestID(evt.Payload),
		IdempotencyKey: ticketIdempotencyKey(evt),
		QueuedAt:       time.Now(),
	}
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
// resolved, merged into another ticket) with the tenant's support address as Reply-To, and agents
// get assignments, mentions, escalations, SLA warnings and SLA breaches.
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

		// An agent reply on a WhatsApp ticket also goes to the customer on WhatsApp, from the
		// tenant's own number, while their 24-hour window is open. The email below still goes
		// when the requester has an address.
		if evt.EventType == "ticket.comment_added" {
			if err := dispatchTicketWhatsApp(ctx, nc, cfg, evt, ti, logg); err != nil {
				logg.Error("ticketing event: failed to dispatch whatsapp reply", zap.Error(err))
				_ = m.Nak()
				return
			}
		}

		to, target, replyTo, ok := ticketRecipients(mapping, evt.Payload, ti)
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
		msg := ticketMessage(mapping, evt, to, target, replyTo, links)

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
