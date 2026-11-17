package main

import (
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// Client portal notices (projects-api sprint 14). Each event carries one recipient:
//
//   - project.client.invited: the firm's client, by the email and phone projects-api read from
//     the CRM (the person has no account yet). The button opens the project in the client portal,
//     where they sign in.
//   - project.milestone.ready_for_review: one client of the project. A signed-in client by
//     recipient_user_id; a client still at the invite stage has an empty recipient_user_id and
//     the email and phone projects-api read from the CRM at send time, used as for an invite.
//   - project.milestone.approved and project.milestone.changes_requested: the project's owner and
//     each owner or manager member, never the client.
//
// Like the task notices, the email goes to recipient_email (else the local user table) and
// WhatsApp to the phone (the payload's for an invite, else the user table's). All four are
// essential in the preferences registry.

// projectPortalPayload is the payload of a portal notice.
type projectPortalPayload struct {
	ProjectID       string
	ProjectName     string
	MilestoneName   string
	TargetDate      string
	ActorName       string
	RecipientUserID string
	RecipientEmail  string
	RecipientPhone  string
	RecipientName   string
	CommentExcerpt  string
	ActionPath      string
}

func readProjectPortalPayload(p map[string]any) projectPortalPayload {
	return projectPortalPayload{
		ProjectID:       mStr(p, "project_id"),
		ProjectName:     oneLine(mStr(p, "project_name")),
		MilestoneName:   oneLine(mStr(p, "milestone_name")),
		TargetDate:      mStr(p, "target_date"),
		ActorName:       oneLine(mStr(p, "actor_name")),
		RecipientUserID: mStr(p, "recipient_user_id"),
		RecipientEmail:  mStr(p, "recipient_email"),
		RecipientPhone:  mStr(p, "recipient_phone"),
		RecipientName:   oneLine(mStr(p, "recipient_name")),
		CommentExcerpt:  capRunes(oneLine(mStr(p, "comment_excerpt")), projectTaskExcerptMax),
		ActionPath:      mStr(p, "action_path"),
	}
}

// projectPortalNotice is how one portal event is worded.
type projectPortalNotice struct {
	TemplateID string
	Target     string // messaging.Target*
	Subject    func(p projectPortalPayload, org string) string
	WhatsApp   func(p projectPortalPayload, org string) (string, []string)
}

func portalProject(p projectPortalPayload) string   { return waParam(p.ProjectName, "your project") }
func portalMilestone(p projectPortalPayload) string { return waParam(p.MilestoneName, "a milestone") }
func portalClient(p projectPortalPayload) string    { return waParam(p.ActorName, "Your client") }

var projectPortalNotices = map[string]projectPortalNotice{
	"client.invited": {
		TemplateID: "projects/client_invited",
		Target:     messaging.TargetCustomer,
		Subject: func(p projectPortalPayload, org string) string {
			return org + " invited you to follow " + portalProject(p)
		},
		WhatsApp: func(p projectPortalPayload, org string) (string, []string) {
			return "projects_client_invited_v1_btn", []string{waParam(p.RecipientName, "there"), org, portalProject(p)}
		},
	},
	"milestone.ready_for_review": {
		TemplateID: "projects/milestone_ready_for_review",
		Target:     messaging.TargetCustomer,
		Subject: func(p projectPortalPayload, _ string) string {
			return portalMilestone(p) + " is ready for your review"
		},
		WhatsApp: func(p projectPortalPayload, _ string) (string, []string) {
			return "projects_milestone_review_v1_btn", []string{waParam(p.RecipientName, "there"), portalMilestone(p), portalProject(p)}
		},
	},
	"milestone.approved": {
		TemplateID: "projects/milestone_approved",
		Target:     messaging.TargetStaff,
		Subject: func(p projectPortalPayload, _ string) string {
			return portalClient(p) + " approved " + portalMilestone(p)
		},
		WhatsApp: func(p projectPortalPayload, _ string) (string, []string) {
			params := []string{waParam(p.RecipientName, "there"), portalClient(p), portalMilestone(p), portalProject(p)}
			if p.CommentExcerpt == "" {
				return "projects_milestone_approved_short_v1_btn", params
			}
			return "projects_milestone_approved_v1_btn", append(params, p.CommentExcerpt)
		},
	},
	"milestone.changes_requested": {
		TemplateID: "projects/milestone_changes_requested",
		Target:     messaging.TargetStaff,
		Subject: func(p projectPortalPayload, _ string) string {
			return portalClient(p) + " asked for changes to " + portalMilestone(p)
		},
		WhatsApp: func(p projectPortalPayload, _ string) (string, []string) {
			return "projects_milestone_changes_v1_btn", []string{waParam(p.RecipientName, "there"), portalClient(p),
				portalMilestone(p), portalProject(p), waParam(p.CommentExcerpt, "See the request in Projects")}
		},
	},
}

// buildProjectPortalNotice turns a portal event into a notice for its one recipient. userEmail and
// userPhone come from the local user table for recipient_user_id ("" when unknown); an invite's
// own email and phone come first. ok is false when the recipient cannot be reached at all.
func buildProjectPortalNotice(evt projectsEvent, kind string, pn projectPortalNotice, ti *tenantInfo, userEmail, userPhone string,
	gate *preferences.Gate, templateChannels map[string][]string) (partyNotice, bool) {
	p := readProjectPortalPayload(evt.Payload)
	email := p.RecipientEmail
	if email == "" {
		email = userEmail
	}
	if !sendableEmail(email) {
		email = ""
	}
	phone := p.RecipientPhone
	if phone == "" {
		phone = userPhone
	}
	if email == "" && phone == "" {
		return partyNotice{}, false
	}
	org := waParam(ti.Name, waParam(p.ActorName, "Your project team"))
	link := projectTaskLink(ti, p.ActionPath)
	data := map[string]any{
		"name":           waParam(p.RecipientName, "there"),
		"organisation":   org,
		"actor_name":     waParam(p.ActorName, org),
		"project_name":   portalProject(p),
		"milestone_name": portalMilestone(p),
		"project_id":     p.ProjectID,
		"action_link":    link,
	}
	if kind == "client.invited" {
		data["email"] = email
	} else if kind != "milestone.ready_for_review" {
		data["actor_name"] = portalClient(p)
	}
	if p.TargetDate != "" {
		data["target_date"] = p.TargetDate
	}
	if p.CommentExcerpt != "" {
		data["comment_excerpt"] = p.CommentExcerpt
	}
	waName, waParams := pn.WhatsApp(p, org)
	return partyNotice{
		ServiceID:  "projects",
		TenantID:   evt.TenantID,
		TemplateID: pn.TemplateID,
		Subject:    pn.Subject(p, org),
		Scope:      messaging.SenderScopeTenant,
		Data:       data,
		Country:    ti.Country,
		IdemBase:   projectTaskIdemBase(evt, kind),
		Parties: []noticeParty{{
			Target:   pn.Target,
			Email:    email,
			Phone:    phone,
			WhatsApp: &partyWhatsApp{Name: waName, Params: waParams, Button: buttonURLSuffix(link)},
		}},
		Gate:             gate,
		TemplateChannels: templateChannels,
	}, true
}
