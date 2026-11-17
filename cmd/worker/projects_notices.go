package main

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/notifications-api/internal/ent"
	entuser "github.com/bengobox/notifications-api/internal/ent/user"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// Client thread, change request and automation notices (projects-api, 2026-11-17). Each goes by
// email and, since 2026-11-17, by WhatsApp too: one UTILITY _btn template per notice in
// internal/whatsapp/templatesync/templates.json with its text in templates/whatsapp/projects. Until
// Meta approves a template on the sending WABA the WhatsApp send fails and only the email lands, as
// for every other _btn template.
//
//   - project.client_thread.posted: a client wrote on the client thread; one event per project
//     owner or owner or manager member, never a client.
//   - project.client_thread.replied: the team wrote on the client thread; one event per signed-in
//     client of the project.
//   - project.change_request.review_requested: a change request waits for approval; one event per
//     approver candidate, never the requester.
//   - project.change_request.decided: the requester's change request was approved or rejected.
//   - project.task.automation_notice: a status automation's notify action. One event per rule run
//     carries every recipient id; each id is looked up in the local user table here and gets its
//     own email and WhatsApp message, keyed per event and recipient, and an unknown id is skipped.
//
// The one-recipient notices read the recipient like the task notices: recipient_email, else the
// local user's email for recipient_user_id; recipient_phone, else the local user's phone. The
// WhatsApp bodies carry facts only (names, the project, the request title, the new status), never
// the free text of a post or comment: a body that is mostly free text is billed as marketing.

// projectNoticePayload is the payload of a client thread, change request or automation notice.
type projectNoticePayload struct {
	ProjectID       string
	ProjectName     string
	ActorName       string
	RecipientUserID string
	RecipientEmail  string
	RecipientPhone  string
	RecipientName   string
	CommentExcerpt  string
	ActionPath      string
	// Change requests.
	Title    string
	Kind     string
	Decision string
	// Status reports (projects_reports.go).
	ReportTitle string
	PeriodStart string
	PeriodEnd   string
	RAGOverall  string
	// Tender notices (projects_reports.go).
	TenderNumber   string
	TenderTitle    string
	ClientName     string
	Section        string
	Recommendation string
}

func readProjectNoticePayload(p map[string]any) projectNoticePayload {
	return projectNoticePayload{
		ProjectID:       mStr(p, "project_id"),
		ProjectName:     oneLine(mStr(p, "project_name")),
		ActorName:       oneLine(mStr(p, "actor_name")),
		RecipientUserID: mStr(p, "recipient_user_id"),
		RecipientEmail:  mStr(p, "recipient_email"),
		RecipientPhone:  strings.TrimSpace(mStr(p, "recipient_phone")),
		RecipientName:   oneLine(mStr(p, "recipient_name")),
		CommentExcerpt:  capRunes(oneLine(mStr(p, "comment_excerpt")), projectTaskExcerptMax),
		ActionPath:      mStr(p, "action_path"),
		Title:           oneLine(mStr(p, "title")),
		Kind:            mStr(p, "kind"),
		Decision:        mStr(p, "decision"),
		ReportTitle:     oneLine(mStr(p, "report_title")),
		PeriodStart:     mStr(p, "period_start"),
		PeriodEnd:       mStr(p, "period_end"),
		RAGOverall:      mStr(p, "rag_overall"),
		TenderNumber:    oneLine(mStr(p, "tender_number")),
		TenderTitle:     oneLine(mStr(p, "tender_title")),
		ClientName:      oneLine(mStr(p, "client_name")),
		Section:         oneLine(mStr(p, "section")),
		Recommendation:  mStr(p, "recommendation"),
	}
}

// projectNotice is how one client thread or change request notice is worded.
type projectNotice struct {
	TemplateID string
	Target     string // messaging.Target*
	Subject    func(p projectNoticePayload, org string) string
	// WhatsApp picks the template and its body parameters.
	WhatsApp func(p projectNoticePayload, org string) (string, []string)
}

func noticeProject(p projectNoticePayload) string { return waParam(p.ProjectName, "your project") }
func noticeRequest(p projectNoticePayload) string { return waParam(p.Title, "a change request") }
func noticeName(p projectNoticePayload) string    { return waParam(p.RecipientName, "there") }

// changeKindLabels name a change request's kind in a sentence.
var changeKindLabels = map[string]string{"scope": "scope", "cost": "cost", "schedule": "schedule"}

// WhatsApp templates of these notices.
const (
	waClientThreadPosted  = "projects_client_thread_posted_v1_btn"
	waClientThreadReplied = "projects_client_thread_replied_v1_btn"
	waChangeReview        = "projects_change_request_review_v1_btn"
	waChangeDecided       = "projects_change_request_decided_v1_btn"
	waTaskAutomation      = "projects_task_automation_v1_btn"
)

var projectNotices = map[string]projectNotice{
	"client_thread.posted": {
		TemplateID: "projects/client_thread_posted",
		Target:     messaging.TargetStaff,
		Subject: func(p projectNoticePayload, _ string) string {
			return waParam(p.ActorName, "Your client") + " wrote on the " + noticeProject(p) + " client thread"
		},
		WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
			return waClientThreadPosted, []string{noticeName(p), waParam(p.ActorName, "Your client"), noticeProject(p)}
		},
	},
	"client_thread.replied": {
		TemplateID: "projects/client_thread_replied",
		Target:     messaging.TargetCustomer,
		Subject: func(p projectNoticePayload, org string) string {
			return org + " replied on " + noticeProject(p)
		},
		WhatsApp: func(p projectNoticePayload, org string) (string, []string) {
			return waClientThreadReplied, []string{noticeName(p), org, noticeProject(p)}
		},
	},
	"change_request.review_requested": {
		TemplateID: "projects/change_request_review_requested",
		Target:     messaging.TargetStaff,
		Subject: func(p projectNoticePayload, _ string) string {
			return "Change request to approve: " + noticeRequest(p)
		},
		WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
			return waChangeReview, []string{noticeName(p), waParam(p.ActorName, "A teammate"), noticeRequest(p), noticeProject(p)}
		},
	},
	"change_request.decided": {
		TemplateID: "projects/change_request_decided",
		Target:     messaging.TargetStaff,
		Subject: func(p projectNoticePayload, _ string) string {
			return "Your change request was " + changeDecisionWord(p.Decision) + ": " + noticeRequest(p)
		},
		WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
			return waChangeDecided, []string{noticeName(p), noticeRequest(p), noticeProject(p), changeDecisionWord(p.Decision)}
		},
	},
}

// changeDecisionWord is the decision as the requester reads it.
func changeDecisionWord(decision string) string {
	if decision == "approved" {
		return "approved"
	}
	return "rejected"
}

// buildProjectNotice turns a client thread or change request notice into a notice for its one
// recipient: the email goes to recipient_email, else userEmail, and WhatsApp to recipient_phone,
// else userPhone (both from the local user table). ok is false when the recipient can be reached
// neither way.
func buildProjectNotice(evt projectsEvent, kind string, en projectNotice, ti *tenantInfo, userEmail, userPhone string,
	gate *preferences.Gate, templateChannels map[string][]string) (partyNotice, bool) {
	p := readProjectNoticePayload(evt.Payload)
	email := p.RecipientEmail
	if email == "" {
		email = userEmail
	}
	if !sendableEmail(email) {
		email = ""
	}
	phone := p.RecipientPhone
	if phone == "" {
		phone = strings.TrimSpace(userPhone)
	}
	if email == "" && phone == "" {
		return partyNotice{}, false
	}
	org := waParam(ti.Name, "Your project team")
	link := projectTaskLink(ti, p.ActionPath)
	data := map[string]any{
		"name":         noticeName(p),
		"organisation": org,
		"actor_name":   waParam(p.ActorName, "A teammate"),
		"project_name": noticeProject(p),
		"project_id":   p.ProjectID,
		"action_link":  link,
	}
	switch kind {
	case "client_thread.posted":
		data["actor_name"] = waParam(p.ActorName, "Your client")
	case "client_thread.replied":
		data["actor_name"] = waParam(p.ActorName, org)
	case "change_request.review_requested", "change_request.decided":
		data["title"] = noticeRequest(p)
		if label, ok := changeKindLabels[p.Kind]; ok {
			data["kind"] = label
		}
		if kind == "change_request.decided" {
			data["decision"] = changeDecisionWord(p.Decision)
			data["approved"] = p.Decision == "approved"
		}
	default:
		// Status report and tender notices (projects_reports.go).
		reportAndTenderData(kind, p, data)
	}
	if p.CommentExcerpt != "" {
		data["comment_excerpt"] = p.CommentExcerpt
	}
	waName, waParams := en.WhatsApp(p, org)
	return partyNotice{
		ServiceID:  "projects",
		TenantID:   evt.TenantID,
		TemplateID: en.TemplateID,
		Subject:    en.Subject(p, org),
		Scope:      messaging.SenderScopeTenant,
		Data:       data,
		Country:    ti.Country,
		IdemBase:   projectTaskIdemBase(evt, kind),
		Parties: []noticeParty{{
			Target:   en.Target,
			Email:    email,
			Phone:    phone,
			WhatsApp: &partyWhatsApp{Name: waName, Params: waParams, Button: buttonURLSuffix(link)},
		}},
		Gate:             gate,
		TemplateChannels: templateChannels,
	}, true
}

// Automation notices.

// projectAutomationTemplate is the automation notice's email and WhatsApp text template.
const projectAutomationTemplate = "projects/task_automation_notice"

// projectAutomationMaxRecipients caps one rule run's fan-out (projects-api allows far fewer).
const projectAutomationMaxRecipients = 50

// automationRecipient is one person an automation notice goes to, from the local user table.
type automationRecipient struct {
	ID    string
	Email string
	Phone string
	Name  string
}

// projectAutomationRecipientIDs reads recipient_ids: distinct, parseable, non-nil auth user ids,
// in order, at most projectAutomationMaxRecipients.
func projectAutomationRecipientIDs(p map[string]any) []uuid.UUID {
	raw, _ := p["recipient_ids"].([]any)
	seen := map[uuid.UUID]bool{}
	out := make([]uuid.UUID, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil || id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == projectAutomationMaxRecipients {
			break
		}
	}
	return out
}

// lookupAutomationRecipients reads the recipients' email, phone and name from the local user table
// in one query, preferring the row of the event's tenant when a person has several. An id with no
// row, or with neither a sendable email nor a phone, is left out.
func lookupAutomationRecipients(ctx context.Context, client *ent.Client, tenantID string, ids []uuid.UUID) ([]automationRecipient, error) {
	if client == nil || len(ids) == 0 {
		return nil, nil
	}
	rows, err := client.User.Query().Where(entuser.AuthServiceUserIDIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	return pickAutomationRecipients(rows, tenantID, ids), nil
}

// pickAutomationRecipients picks, for each id in order, its user row (the event tenant's first)
// and keeps those with a sendable email or a phone.
func pickAutomationRecipients(rows []*ent.User, tenantID string, ids []uuid.UUID) []automationRecipient {
	tenant, _ := uuid.Parse(tenantID)
	best := map[uuid.UUID]*ent.User{}
	for _, u := range rows {
		if prev, ok := best[u.AuthServiceUserID]; !ok || (prev.TenantID != tenant && u.TenantID == tenant) {
			best[u.AuthServiceUserID] = u
		}
	}
	out := make([]automationRecipient, 0, len(ids))
	for _, id := range ids {
		u, ok := best[id]
		if !ok {
			continue
		}
		email, phone := strings.TrimSpace(u.Email), strings.TrimSpace(u.Phone)
		if !sendableEmail(email) {
			email = ""
		}
		if email == "" && phone == "" {
			continue
		}
		out = append(out, automationRecipient{ID: id.String(), Email: email, Phone: phone, Name: oneLine(u.FullName)})
	}
	return out
}

// buildProjectAutomationNotice turns one automation notice into one party per recipient, by email
// and WhatsApp. Each party's Key is its user id, so the idempotency key is per event, channel and
// recipient (projects-task.automation_notice-<event id>-email-<user id>). The rule's own message
// goes in the email only. ok is false with no recipient.
func buildProjectAutomationNotice(evt projectsEvent, ti *tenantInfo, recipients []automationRecipient,
	gate *preferences.Gate, templateChannels map[string][]string) (partyNotice, bool) {
	if len(recipients) == 0 {
		return partyNotice{}, false
	}
	p := evt.Payload
	task := waParam(oneLine(mStr(p, "task_title")), "A task")
	status := waParam(oneLine(mStr(p, "to_status_label")), waParam(mStr(p, "to_status"), "a new status"))
	project := waParam(oneLine(mStr(p, "project_name")), "your project")
	link := projectTaskLink(ti, mStr(p, "action_path"))
	data := map[string]any{
		"project_name": project,
		"project_id":   mStr(p, "project_id"),
		"task_id":      mStr(p, "task_id"),
		"task_title":   task,
		"status_label": status,
		"rule_name":    oneLine(mStr(p, "rule_name")),
		"action_link":  link,
	}
	if msg := capRunes(oneLine(mStr(p, "message")), 500); msg != "" {
		data["message"] = msg
	}
	button := buttonURLSuffix(link)
	parties := make([]noticeParty, 0, len(recipients))
	for _, r := range recipients {
		name := waParam(r.Name, "there")
		parties = append(parties, noticeParty{
			Key:    r.ID,
			Target: messaging.TargetStaff,
			Email:  r.Email,
			Phone:  r.Phone,
			Data:   map[string]any{"name": name},
			WhatsApp: &partyWhatsApp{Name: waTaskAutomation, Params: []string{name, task, project, status},
				Button: button},
		})
	}
	return partyNotice{
		ServiceID:        "projects",
		TenantID:         evt.TenantID,
		TemplateID:       projectAutomationTemplate,
		Subject:          task + " moved to " + status,
		Scope:            messaging.SenderScopeTenant,
		Data:             data,
		Country:          ti.Country,
		IdemBase:         projectTaskIdemBase(evt, "task.automation_notice"),
		Parties:          parties,
		Gate:             gate,
		TemplateChannels: templateChannels,
	}, true
}
