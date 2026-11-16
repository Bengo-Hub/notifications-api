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
	"github.com/bengobox/notifications-api/internal/ent"
	entuser "github.com/bengobox/notifications-api/internal/ent/user"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// projectsEvent is the shared-events envelope from projects-api.
type projectsEvent struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	AggregateType string         `json:"aggregate_type"`
	AggregateID   string         `json:"aggregate_id"`
	EventType     string         `json:"event_type"`
	Payload       map[string]any `json:"payload"`
	Timestamp     string         `json:"timestamp"`
	Version       string         `json:"version"`
}

type projectsNotificationMapping struct {
	TemplateID   string
	EmailSubject string
	DataBuilder  func(payload map[string]any, tenantWebsite string) map[string]any
}

var projectsMappings = map[string]projectsNotificationMapping{
	"milestone.reached": {
		TemplateID:   "projects/project_milestone_reached",
		EmailSubject: "Project milestone reached",
		DataBuilder: func(payload map[string]any, tenantWebsite string) map[string]any {
			return map[string]any{
				"name":             "Team",
				"project_name":     payload["project_name"],
				"milestone_name":   payload["milestone_name"],
				"completion_date":  formatEventDate(payload["completion_date"]),
				"project_progress": payload["progress"], // the template reads project_progress
				"action_link":      fmt.Sprintf("%s/projects/%s", tenantWebsite, payload["project_id"]),
			}
		},
	},
}

// Task notices (projects-api sprint 12): project.task.mentioned and project.task.assigned carry one
// recipient each, a teammate other than the author. The recipient gets an email at
// recipient_email and a WhatsApp message on the phone in the local user table (synced from
// auth-api) for recipient_user_id; with no phone only the email goes. Both types are essential
// in the preferences registry (on by default), and the tenant's choices apply before anything
// is queued.

// projectsAppBase is the projects app; its WhatsApp buttons are fixed to this domain.
const projectsAppBase = "https://projects.codevertexafrica.com"

// projectTaskExcerptMax caps the comment excerpt (projects-api already sends at most 200).
const projectTaskExcerptMax = 200

// projectTaskPayload is the payload of a project.task.* notice.
type projectTaskPayload struct {
	ProjectID       string
	ProjectName     string
	TaskID          string
	TaskTitle       string
	ActorName       string
	RecipientUserID string
	RecipientEmail  string
	RecipientName   string
	CommentExcerpt  string
	ActionPath      string
}

func readProjectTaskPayload(p map[string]any) projectTaskPayload {
	return projectTaskPayload{
		ProjectID:       mStr(p, "project_id"),
		ProjectName:     oneLine(mStr(p, "project_name")),
		TaskID:          mStr(p, "task_id"),
		TaskTitle:       oneLine(mStr(p, "task_title")),
		ActorName:       oneLine(mStr(p, "actor_name")),
		RecipientUserID: mStr(p, "recipient_user_id"),
		RecipientEmail:  mStr(p, "recipient_email"),
		RecipientName:   oneLine(mStr(p, "recipient_name")),
		CommentExcerpt:  capRunes(oneLine(mStr(p, "comment_excerpt")), projectTaskExcerptMax),
		ActionPath:      mStr(p, "action_path"),
	}
}

// oneLine folds every run of spaces, tabs and line breaks into one space: Meta refuses a template
// parameter with a line break, and a subject line has none either.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func capRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return strings.TrimSpace(string(r[:n])) + "..."
	}
	return s
}

// projectTaskNotice is how one task event is worded.
type projectTaskNotice struct {
	TemplateID string
	Subject    func(p projectTaskPayload) string
	// WhatsApp picks the template (a variant per optional line) and its body parameters.
	WhatsApp func(p projectTaskPayload) (string, []string)
}

var projectTaskNotices = map[string]projectTaskNotice{
	"task.mentioned": {
		TemplateID: "projects/task_mentioned",
		Subject: func(p projectTaskPayload) string {
			return projectTaskActor(p) + " mentioned you on " + projectTaskTitle(p)
		},
		WhatsApp: func(p projectTaskPayload) (string, []string) {
			params := []string{waParam(p.RecipientName, "there"), projectTaskActor(p), projectTaskTitle(p), waParam(p.ProjectName, "your project")}
			if p.CommentExcerpt == "" {
				return "projects_task_mentioned_short_v1_btn", params
			}
			return "projects_task_mentioned_v1_btn", append(params, p.CommentExcerpt)
		},
	},
	"task.assigned": {
		TemplateID: "projects/task_assigned",
		Subject: func(p projectTaskPayload) string {
			return projectTaskActor(p) + " assigned you " + projectTaskTitle(p)
		},
		WhatsApp: func(p projectTaskPayload) (string, []string) {
			return "projects_task_assigned_v1_btn", []string{waParam(p.RecipientName, "there"), projectTaskActor(p), projectTaskTitle(p), waParam(p.ProjectName, "your project")}
		},
	},
}

func projectTaskActor(p projectTaskPayload) string { return waParam(p.ActorName, "A teammate") }
func projectTaskTitle(p projectTaskPayload) string { return waParam(p.TaskTitle, "a task") }

// projectTaskLink is the task in the projects app: the tenant's own projects URL when it has one,
// else the shared app, plus action_path. Empty when the event carries no path.
func projectTaskLink(ti *tenantInfo, actionPath string) string {
	path := strings.TrimLeft(strings.TrimSpace(actionPath), "/")
	if path == "" {
		return ""
	}
	return ti.ServiceURL("projects", "NOTIFICATIONS_PROJECTS_APP_URL", projectsAppBase) + "/" + path
}

// projectTaskIdemBase is one per event, so a redelivered event is dropped by its Nats-Msg-Id.
func projectTaskIdemBase(evt projectsEvent, kind string) string {
	id := evt.ID
	if id == "" {
		id = evt.AggregateID + "-" + evt.Timestamp
	}
	return fmt.Sprintf("projects-%s-%s", kind, id)
}

// buildProjectTaskNotice turns a task event into a notice for its one recipient. The email goes to
// recipient_email, else userEmail (the user table); phone is the recipient's number ("" sends the
// email only). ok is false when the recipient cannot be reached at all.
func buildProjectTaskNotice(evt projectsEvent, kind string, tn projectTaskNotice, ti *tenantInfo, userEmail, phone string,
	gate *preferences.Gate, templateChannels map[string][]string) (partyNotice, bool) {
	p := readProjectTaskPayload(evt.Payload)
	email := p.RecipientEmail
	if email == "" {
		email = userEmail
	}
	if !sendableEmail(email) {
		email = ""
	}
	if email == "" && phone == "" {
		return partyNotice{}, false
	}
	link := projectTaskLink(ti, p.ActionPath)
	data := map[string]any{
		"name":        waParam(p.RecipientName, "there"),
		"actor_name":  projectTaskActor(p),
		"task_title":  projectTaskTitle(p),
		"project_id":  p.ProjectID,
		"task_id":     p.TaskID,
		"action_link": link,
	}
	if p.ProjectName != "" {
		data["project_name"] = p.ProjectName
	}
	if p.CommentExcerpt != "" && kind == "task.mentioned" {
		data["comment_excerpt"] = p.CommentExcerpt
	}
	waName, waParams := tn.WhatsApp(p)
	party := noticeParty{
		Target:   messaging.TargetStaff,
		Email:    email,
		Phone:    phone,
		WhatsApp: &partyWhatsApp{Name: waName, Params: waParams, Button: buttonURLSuffix(link)},
	}
	return partyNotice{
		ServiceID:        "projects",
		TenantID:         evt.TenantID,
		TemplateID:       tn.TemplateID,
		Subject:          tn.Subject(p),
		Scope:            messaging.SenderScopeTenant,
		Data:             data,
		Country:          ti.Country,
		IdemBase:         projectTaskIdemBase(evt, kind),
		Parties:          []noticeParty{party},
		Gate:             gate,
		TemplateChannels: templateChannels,
	}, true
}

// projectTaskRecipient reads the recipient's email and phone from the local user table by auth
// user id. Either is "" when unknown; a lookup failure never stops the email in the payload.
func projectTaskRecipient(ctx context.Context, client *ent.Client, userID string) (email, phone string) {
	id, err := uuid.Parse(strings.TrimSpace(userID))
	if err != nil || id == uuid.Nil || client == nil {
		return "", ""
	}
	u, err := client.User.Query().Where(entuser.AuthServiceUserIDEQ(id)).First(ctx)
	if err != nil || u == nil {
		return "", ""
	}
	return strings.TrimSpace(u.Email), strings.TrimSpace(u.Phone)
}

// startProjectsConsumer subscribes to project.> events and dispatches project notifications: the
// milestone email to the tenant's contact, and task mentions and assignments to their recipient.
func startProjectsConsumer(ctx context.Context, nc *nats.Conn, js nats.JetStreamContext, cfg *config.Config, tr *tenantResolver,
	client *ent.Client, gate *preferences.Gate, templateChannels map[string][]string, logg *zap.Logger) {
	if nc == nil || js == nil {
		logg.Warn("skipping projects consumer: NATS not available")
		return
	}

	handler := func(m *nats.Msg) {
		var evt projectsEvent
		if err := json.Unmarshal(m.Data, &evt); err != nil {
			logg.Error("projects event: unmarshal failed", zap.Error(err))
			_ = m.Ack()
			return
		}

		kind := strings.TrimPrefix(evt.EventType, "project.")
		mapping, ok := projectsMappings[kind]
		taskNotice, isTask := projectTaskNotices[kind]
		if !ok && !isTask {
			// qa.* (QA acceptance) is sent by the events subscriber's own durable,
			// notifications-projects-qa (internal/modules/events/project_qa.go), so it lands here.
			logg.Debug("projects event: unhandled type, skipping", zap.String("type", evt.EventType))
			_ = m.Ack()
			return
		}

		tenantID := evt.TenantID
		if tenantID == "" {
			logg.Warn("projects event: no tenant_id, skipping")
			_ = m.Ack()
			return
		}

		ti, err := tr.resolve(ctx, tenantID)
		if err != nil {
			logg.Error("projects event: failed to resolve tenant", zap.String("tenant_id", tenantID), zap.Error(err))
			_ = m.Nak()
			return
		}

		if isTask {
			userEmail, phone := projectTaskRecipient(ctx, client, mStr(evt.Payload, "recipient_user_id"))
			notice, reachable := buildProjectTaskNotice(evt, kind, taskNotice, ti, userEmail, phone, gate, templateChannels)
			if !reachable {
				logg.Info("projects event: task recipient has no email or phone, skipping", zap.String("type", evt.EventType))
				_ = m.Ack()
				return
			}
			if err := sendToParties(ctx, nc, cfg, notice); err != nil {
				logg.Error("projects event: failed to dispatch task notice", zap.String("type", evt.EventType), zap.Error(err))
				_ = m.Nak()
				return
			}
			logg.Info("projects task notice dispatched", zap.String("type", evt.EventType), zap.String("template", taskNotice.TemplateID),
				zap.Bool("whatsapp", phone != ""))
			_ = m.Ack()
			return
		}

		if ti.ContactEmail == "" {
			logg.Warn("projects event: tenant has no contact_email, skipping")
			_ = m.Ack()
			return
		}

		msg := messaging.Message{
			TenantID:    tenantID,
			Channel:     "email",
			TemplateID:  mapping.TemplateID,
			SenderScope: messaging.SenderScopeTenant,
			Target:      messaging.TargetStaff,
			To:          []string{ti.ContactEmail},
			Data:        mapping.DataBuilder(evt.Payload, ti.Website),
			Metadata: map[string]any{
				"subject": mapping.EmailSubject,
			},
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("projects-%s-%s", evt.EventType, evt.AggregateID),
			QueuedAt:       time.Now(),
		}

		if _, err := messaging.Publish(ctx, nc, cfg.Events, msg); err != nil {
			logg.Error("projects event: failed to dispatch notification", zap.String("type", evt.EventType), zap.Error(err))
			_ = m.Nak()
			return
		}

		logg.Info("projects notification dispatched", zap.String("type", evt.EventType), zap.String("template", mapping.TemplateID))
		_ = m.Ack()
	}

	// projects-api publishes every project event on its own "projects" stream.
	eventslib.SubscribeQueueWithRebind(logg, js, "projects", "project.>", "notifications-projects", handler,
		nats.BindStream("projects"),
		nats.Durable("notifications-projects"),
		nats.ManualAck(),
		nats.AckWait(30*time.Second),
		nats.MaxDeliver(3),
	)
}
