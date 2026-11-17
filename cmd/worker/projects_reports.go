package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// Status report, tender and weekly digest notices (projects-api sprint 7, 2026-11-17). Each event
// carries one recipient, read like the other projects notices (recipient_email, else the local
// user's email; recipient_phone, else the local user's phone), and goes by email and WhatsApp: one
// UTILITY _btn template per notice in internal/whatsapp/templatesync/templates.json with its text in
// templates/whatsapp/projects. The WhatsApp bodies carry facts only; free text (a loss reason, a
// review comment, the report's narrative) stays in the email.
//
//   - project.status_report.shared: a published status report was shared with the project's
//     clients; one event per client (a signed-in client, or an invited one with the address and
//     phone projects-api read from marketflow). The button opens the report in the client portal.
//   - project.tender.notice.go_decision: the bid decision, to the committee and the tender's owner.
//   - project.tender.notice.section_review: a section waits for its reviewer (else the owner).
//   - project.tender.notice.section_reviewed: the section's assignee hears the verdict.
//   - project.tender.notice.won, .lost: the outcome, to the committee and the owner.
//   - project.tender.notice.converted: the won tender became a project, to the committee and owner.
//   - project.digest.weekly: one person's weekly summary across their open projects (digest.go).

// WhatsApp templates of these notices.
const (
	waStatusReportShared  = "projects_status_report_shared_v1_btn"
	waTenderDecision      = "projects_tender_decision_v1_btn"
	waTenderSectionReview = "projects_tender_section_review_v1_btn"
	waTenderSectionDone   = "projects_tender_section_reviewed_v1_btn"
	waTenderWon           = "projects_tender_won_v1_btn"
	waTenderLost          = "projects_tender_lost_v1_btn"
	waTenderConverted     = "projects_tender_converted_v1_btn"
	waDigestWeekly        = "projects_digest_weekly_v1_btn"
)

// noticeTender names a tender as its number and title ("TND-2026-0007 County roads").
func noticeTender(p projectNoticePayload) string {
	name := strings.TrimSpace(p.TenderNumber + " " + p.TenderTitle)
	return waParam(name, "your tender")
}

func noticeSection(p projectNoticePayload) string { return waParam(p.Section, "a section") }
func noticeClient(p projectNoticePayload) string  { return waParam(p.ClientName, "the client") }
func noticeReport(p projectNoticePayload) string {
	return waParam(p.ReportTitle, "the latest status report")
}

// tenderDecisionWord is a go/no-go decision as a reader says it.
func tenderDecisionWord(decision string) string {
	if decision == "no_go" {
		return "no go"
	}
	return "go"
}

// sectionVerdictWord is a section review verdict as the assignee reads it.
func sectionVerdictWord(decision string) string {
	if decision == "approved" {
		return "approved"
	}
	return "sent back for changes"
}

// ragWords names a RAG rating.
var ragWords = map[string]string{"green": "Green", "amber": "Amber", "red": "Red"}

func init() {
	staff := messaging.TargetStaff
	add := map[string]projectNotice{
		"status_report.shared": {
			TemplateID: "projects/status_report_shared",
			Target:     messaging.TargetCustomer,
			Subject: func(p projectNoticePayload, org string) string {
				return org + " shared a status report on " + noticeProject(p)
			},
			WhatsApp: func(p projectNoticePayload, org string) (string, []string) {
				return waStatusReportShared, []string{noticeName(p), org, noticeReport(p), noticeProject(p)}
			},
		},
		"tender.notice.go_decision": {
			TemplateID: "projects/tender_go_decision",
			Target:     staff,
			Subject: func(p projectNoticePayload, _ string) string {
				return "Bid decision on " + noticeTender(p) + ": " + tenderDecisionWord(p.Decision)
			},
			WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
				return waTenderDecision, []string{noticeName(p), noticeTender(p), tenderDecisionWord(p.Decision), waParam(p.ActorName, "the committee")}
			},
		},
		"tender.notice.section_review": {
			TemplateID: "projects/tender_section_review",
			Target:     staff,
			Subject: func(p projectNoticePayload, _ string) string {
				return "Section to review: " + noticeSection(p) + " (" + noticeTender(p) + ")"
			},
			WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
				return waTenderSectionReview, []string{noticeName(p), waParam(p.ActorName, "A teammate"), noticeSection(p), noticeTender(p)}
			},
		},
		"tender.notice.section_reviewed": {
			TemplateID: "projects/tender_section_reviewed",
			Target:     staff,
			Subject: func(p projectNoticePayload, _ string) string {
				return "Your section " + noticeSection(p) + " was " + sectionVerdictWord(p.Decision)
			},
			WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
				return waTenderSectionDone, []string{noticeName(p), noticeSection(p), noticeTender(p), sectionVerdictWord(p.Decision)}
			},
		},
		"tender.notice.won": {
			TemplateID: "projects/tender_won",
			Target:     staff,
			Subject:    func(p projectNoticePayload, _ string) string { return "Tender won: " + noticeTender(p) },
			WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
				return waTenderWon, []string{noticeName(p), noticeTender(p), noticeClient(p)}
			},
		},
		"tender.notice.lost": {
			TemplateID: "projects/tender_lost",
			Target:     staff,
			Subject:    func(p projectNoticePayload, _ string) string { return "Tender lost: " + noticeTender(p) },
			WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
				return waTenderLost, []string{noticeName(p), noticeTender(p), noticeClient(p)}
			},
		},
		"tender.notice.converted": {
			TemplateID: "projects/tender_converted",
			Target:     staff,
			Subject: func(p projectNoticePayload, _ string) string {
				return noticeTender(p) + " is now the project " + noticeProject(p)
			},
			WhatsApp: func(p projectNoticePayload, _ string) (string, []string) {
				return waTenderConverted, []string{noticeName(p), noticeTender(p), noticeProject(p)}
			},
		},
	}
	for k, v := range add {
		projectNotices[k] = v
	}
}

// reportAndTenderData adds a status report or tender notice's facts to the email data.
func reportAndTenderData(kind string, p projectNoticePayload, data map[string]any) {
	switch {
	case kind == "status_report.shared":
		data["report_title"] = noticeReport(p)
		if p.PeriodStart != "" && p.PeriodEnd != "" {
			data["period"] = dayText(p.PeriodStart) + " to " + dayText(p.PeriodEnd)
		}
		if w, ok := ragWords[p.RAGOverall]; ok {
			data["rag_overall"] = w
		}
	case strings.HasPrefix(kind, "tender.notice."):
		data["tender"] = noticeTender(p)
		data["client_name"] = noticeClient(p)
		data["section"] = noticeSection(p)
		data["decision_word"] = tenderDecisionWord(p.Decision)
		data["verdict"] = sectionVerdictWord(p.Decision)
		data["approved"] = p.Decision == "approved"
		if p.Recommendation != "" {
			data["recommendation"] = tenderDecisionWord(p.Recommendation)
		}
	}
}

// ---- weekly digest ----

// projectDigestTemplate is the digest's email and WhatsApp text template.
const projectDigestTemplate = "projects/digest_weekly"

// projectDigestItem is one line of a projects digest list (Hadia's digest has its own digestItem).
type projectDigestItem struct {
	Title   string
	Project string
	Due     string
}

func projectDigestItems(p map[string]any, key string) []projectDigestItem {
	raw, _ := p[key].([]any)
	out := make([]projectDigestItem, 0, len(raw))
	for _, v := range raw {
		m, _ := v.(map[string]any)
		if m == nil {
			continue
		}
		it := projectDigestItem{Title: capRunes(oneLine(mStr(m, "title")), 200), Project: oneLine(mStr(m, "project"))}
		if d := mStr(m, "due"); d != "" {
			it.Due = dayText(d)
		}
		if it.Title != "" {
			out = append(out, it)
		}
		if len(out) == 5 {
			break
		}
	}
	return out
}

func digestCount(p map[string]any, key string) int {
	switch v := p[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// buildProjectDigestNotice turns project.digest.weekly into the person's email and WhatsApp
// message. The button and the email link open My Work in the projects app for the tenant. ok is
// false when the person can be reached neither way or the digest is empty.
func buildProjectDigestNotice(evt projectsEvent, ti *tenantInfo, userEmail, userPhone string,
	gate *preferences.Gate, templateChannels map[string][]string) (partyNotice, bool) {
	p := evt.Payload
	email := mStr(p, "recipient_email")
	if email == "" {
		email = userEmail
	}
	if !sendableEmail(email) {
		email = ""
	}
	phone := strings.TrimSpace(mStr(p, "recipient_phone"))
	if phone == "" {
		phone = strings.TrimSpace(userPhone)
	}
	counts := map[string]int{}
	total := 0
	for _, k := range []string{"overdue_count", "due_count", "milestone_count", "risk_count", "approval_count"} {
		counts[k] = digestCount(p, k)
		total += counts[k]
	}
	if (email == "" && phone == "") || total == 0 {
		return partyNotice{}, false
	}
	path := strings.TrimSpace(mStr(p, "action_path"))
	if path == "" && ti.Slug != "" {
		path = "/" + ti.Slug + "/my-work"
	}
	link := projectTaskLink(ti, path)
	name := waParam(oneLine(mStr(p, "recipient_name")), "there")
	org := waParam(ti.Name, "your organisation")
	week := mStr(p, "week")
	data := map[string]any{
		"name":              name,
		"organisation":      org,
		"week":              week,
		"overdue_count":     counts["overdue_count"],
		"due_count":         counts["due_count"],
		"milestone_count":   counts["milestone_count"],
		"risk_count":        counts["risk_count"],
		"approval_count":    counts["approval_count"],
		"overdue":           projectDigestItems(p, "overdue"),
		"due_this_week":     projectDigestItems(p, "due_this_week"),
		"milestones":        projectDigestItems(p, "milestones"),
		"rising_risks":      projectDigestItems(p, "rising_risks"),
		"pending_approvals": projectDigestItems(p, "pending_approvals"),
		"action_link":       link,
	}
	if ws := mStr(p, "week_start"); ws != "" {
		data["week_start"] = dayText(ws)
	}
	params := []string{name, org,
		fmt.Sprint(counts["overdue_count"]), fmt.Sprint(counts["due_count"]), fmt.Sprint(counts["milestone_count"]),
		fmt.Sprint(counts["risk_count"]), fmt.Sprint(counts["approval_count"])}
	subject := "Your projects this week"
	if week != "" {
		subject += " (" + week + ")"
	}
	return partyNotice{
		ServiceID:  "projects",
		TenantID:   evt.TenantID,
		TemplateID: projectDigestTemplate,
		Subject:    subject,
		Scope:      messaging.SenderScopeTenant,
		Data:       data,
		Country:    ti.Country,
		IdemBase:   projectTaskIdemBase(evt, "digest.weekly"),
		Parties: []noticeParty{{
			Target:   messaging.TargetStaff,
			Email:    email,
			Phone:    phone,
			WhatsApp: &partyWhatsApp{Name: waDigestWeekly, Params: params, Button: buttonURLSuffix(link)},
		}},
		Gate:             gate,
		TemplateChannels: templateChannels,
	}, true
}

// dayText shows a YYYY-MM-DD date from projects-api as "09 Nov 2026" (an RFC3339 time too).
func dayText(s string) string {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t.Format("02 Jan 2006")
	}
	return formatEventDate(s)
}
