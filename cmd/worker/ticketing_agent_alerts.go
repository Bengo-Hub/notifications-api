package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// Ticket events added with ticketing-api's agent workspace (Sprint 9) and SLA work (Sprint 10):
//
//   - ticketing.ticket.mentioned (tickets/mentions.go): one event per mentioned agent, sent to that
//     agent's agent_email only. It never falls back to the tenant's support mailbox, because a
//     mention is meant for one person.
//   - ticketing.ticket.merged (tickets/merge.go): carries the source ticket's requester, who is told
//     which ticket now carries their request.
//   - ticketing.sla.breach_warning and ticketing.ticket.escalated (Sprint 10): sent to the agent, or
//     to the addresses the payload gives for the team. With neither they are skipped and logged,
//     never sent to the support mailbox.
//
// Agent alerts carry no requester contact: their data builders copy only ticket fields, even where
// the payload (an escalation built on the shared ticket payload) includes the requester.

// ticketAgentName is the agent's name from the payload, or "Agent" like the other agent alerts.
func ticketAgentName(p map[string]any) string {
	if n := ticketText(p, "agent_name"); n != "" {
		return n
	}
	return "Agent"
}

// ticketConsoleLink is the ticket in the agent console, built the same way as the assignment and
// SLA breach alerts.
func ticketConsoleLink(links ticketLinks, p map[string]any) string {
	return fmt.Sprintf("%s/tickets/%s", links.Console, ticketText(p, "ticket_id"))
}

// ticketEmailList reads email addresses from the first of keys that holds any, as a single string
// or a list. Addresses are trimmed, deduplicated ignoring case, and anything without an "@" is
// dropped.
func ticketEmailList(p map[string]any, keys ...string) []string {
	for _, k := range keys {
		var raw []string
		switch v := p[k].(type) {
		case string:
			raw = strings.Split(v, ",")
		case []string:
			raw = v
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok {
					raw = append(raw, s)
				}
			}
		}
		var out []string
		seen := map[string]bool{}
		for _, e := range raw {
			e = strings.TrimSpace(e)
			if !strings.Contains(e, "@") || seen[strings.ToLower(e)] {
				continue
			}
			seen[strings.ToLower(e)] = true
			out = append(out, e)
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// ticketTeamEmails are the team addresses an SLA warning or escalation goes to when nobody is
// assigned: ticketing-api sends team_emails (the team's supervisors and admins, or all its agents).
func ticketTeamEmails(p map[string]any) []string {
	return ticketEmailList(p, "team_emails")
}

// ticketAgentOrTeamRoute sends to the assigned agent, else the team's addresses, else nobody.
func ticketAgentOrTeamRoute(p map[string]any, _ *tenantInfo) ([]string, string, string, bool) {
	if e := ticketText(p, "agent_email"); e != "" {
		return []string{e}, messaging.TargetStaff, "", true
	}
	if team := ticketTeamEmails(p); len(team) > 0 {
		return team, messaging.TargetStaff, "", true
	}
	return nil, messaging.TargetStaff, "", false
}

// ticketMentionedMapping emails the one agent a comment mentions, with the comment excerpt.
var ticketMentionedMapping = ticketingNotificationMapping{
	TemplateID:   "ticketing/ticket_mentioned",
	EmailSubject: "You were mentioned on a support ticket",
	Audience:     audienceAgent,
	// An agent who mentions themselves is not told about their own comment.
	Skip: func(p map[string]any) bool {
		author, agent := ticketText(p, "author_id"), ticketText(p, "agent_id")
		return author != "" && author == agent
	},
	SubjectBuilder: func(p map[string]any) string {
		if n := ticketText(p, "ticket_number"); n != "" {
			return "You were mentioned on ticket " + n
		}
		return ""
	},
	Route: func(p map[string]any, _ *tenantInfo) ([]string, string, string, bool) {
		e := ticketText(p, "agent_email")
		if e == "" {
			return nil, messaging.TargetStaff, "", false
		}
		return []string{e}, messaging.TargetStaff, "", true
	},
	DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
		return map[string]any{
			"name":          ticketAgentName(p),
			"ticket_id":     ticketText(p, "ticket_number"),
			"subject":       ticketText(p, "subject"),
			"comment":       ticketText(p, "comment"),
			"internal_note": ticketText(p, "comment_type") == "internal",
			"action_link":   ticketConsoleLink(links, p),
		}
	},
}

// ticketMergedMapping tells the requester their ticket was merged and which ticket carries their
// request now. Same route as the other requester notices: their email, Reply-To the tenant's
// support address, nothing when they have no email.
var ticketMergedMapping = ticketingNotificationMapping{
	TemplateID:   "ticketing/ticket_merged",
	EmailSubject: "Your support request has moved to another ticket",
	Audience:     audienceRequester,
	Skip: func(p map[string]any) bool {
		return ticketText(p, "merged_into_number") == ""
	},
	SubjectBuilder: func(p map[string]any) string {
		from, into := ticketText(p, "ticket_number"), ticketText(p, "merged_into_number")
		if from == "" || into == "" {
			return ""
		}
		return fmt.Sprintf("Your request %s is now handled under %s", from, into)
	},
	DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
		d := requesterBase(p, links)
		d["merged_into_id"] = ticketText(p, "merged_into_number")
		return d
	},
}

// ticketSLAKindLabel names the SLA target a warning or escalation is about (payload "target").
func ticketSLAKindLabel(p map[string]any) string {
	v := strings.ToLower(ticketText(p, "target"))
	switch {
	case strings.Contains(v, "resolution"):
		return "Resolution"
	case strings.Contains(v, "response"):
		return "First response"
	}
	return "The SLA target"
}

// ticketMinutesLeft is how many minutes remain before the breach (payload "minutes_left"), or 0
// when the payload does not say.
func ticketMinutesLeft(p map[string]any) int {
	switch v := p["minutes_left"].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ticketTimeLeft is the remaining time in words ("45 minutes", "2 hours"), or "" when unknown.
func ticketTimeLeft(minutes int) string {
	if minutes <= 0 {
		return ""
	}
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	hours, mins := minutes/60, minutes%60
	switch {
	case hours == 0:
		return plural(mins, "minute")
	case mins == 0:
		return plural(hours, "hour")
	}
	return plural(hours, "hour") + " " + plural(mins, "minute")
}

// ticketBreachWarningMapping warns the assigned agent (else the team's supervisors) that a ticket
// is about to pass its SLA target.
var ticketBreachWarningMapping = ticketingNotificationMapping{
	TemplateID:   "ticketing/sla_breach_warning",
	EmailSubject: "A support ticket is close to its SLA target",
	Audience:     audienceAgent,
	SubjectBuilder: func(p map[string]any) string {
		if n := ticketText(p, "ticket_number"); n != "" {
			return "Ticket " + n + " is close to its SLA target"
		}
		return ""
	},
	Route: ticketAgentOrTeamRoute,
	DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
		return map[string]any{
			"name":         ticketAgentName(p),
			"breach_label": ticketSLAKindLabel(p),
			"time_left":    ticketTimeLeft(ticketMinutesLeft(p)),
			"due_at":       formatEventTime(p["due_at"]),
			"ticket_id":    ticketText(p, "ticket_number"),
			"subject":      ticketText(p, "subject"),
			"priority":     ticketPriorityLabel(ticketText(p, "priority")),
			"action_link":  ticketConsoleLink(links, p),
		}
	},
}

// ticketPriorityLabel is a priority in words.
func ticketPriorityLabel(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + strings.ReplaceAll(s[1:], "_", " ")
}

// ticketEscalatedMapping tells the new assignee (else the team's addresses) that a ticket was
// escalated to them.
var ticketEscalatedMapping = ticketingNotificationMapping{
	TemplateID:   "ticketing/ticket_escalated",
	EmailSubject: "A support ticket was escalated to you",
	Audience:     audienceAgent,
	SubjectBuilder: func(p map[string]any) string {
		if n := ticketText(p, "ticket_number"); n != "" {
			return "Ticket " + n + " was escalated to you"
		}
		return ""
	},
	Route: ticketAgentOrTeamRoute,
	DataBuilder: func(p map[string]any, links ticketLinks) map[string]any {
		level := ""
		switch v := p["escalation_level"].(type) {
		case float64:
			level = strconv.Itoa(int(v))
		case int:
			level = strconv.Itoa(v)
		case string:
			level = strings.TrimSpace(v)
		}
		return map[string]any{
			"name":             ticketAgentName(p),
			"ticket_id":        ticketText(p, "ticket_number"),
			"subject":          ticketText(p, "subject"),
			"priority":         ticketPriorityLabel(ticketText(p, "priority")),
			"old_priority":     ticketPriorityLabel(ticketText(p, "old_priority")),
			"escalation_level": level,
			"team_name":        ticketText(p, "team_name"),
			"reason":           ticketText(p, "reason"),
			"action_link":      ticketConsoleLink(links, p),
		}
	},
}
