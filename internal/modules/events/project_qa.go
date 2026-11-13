package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	entuser "github.com/bengobox/notifications-api/internal/ent/user"
	"github.com/bengobox/notifications-api/internal/messaging"
)

// QA acceptance notifications (owner, 13 November 2026). projects-api's QA module
// (internal/services/qa) publishes project.qa.<type> through its outbox with aggregate "project",
// so the events ride the projects stream (project.>). Every payload carries the session's common
// fields (session_id, tenant_id, tenant_slug, tenant_name, session_title, status, scope,
// subject_kind, subject_ref, owner_id, link). scope "platform" is a Codevertex service session:
// its failures go to platform ops on the ops alert path (sendOpsAlert). scope "tenant" (and a
// missing scope) is the tenant's own work: it goes to the tenant's team and never to ops.
//
//   - invite.created: one email to the invited address (Codevertex branded for platform scope, the
//     tenant's branding for tenant scope) with the session, the role and the /qa/{code} link.
//   - result.failed: platform scope to ops by email and Slack, plus the ops WhatsApp numbers for a
//     severity 1 failure; tenant scope by email to the session owner and the tenant's contact.
//   - session.closed, session.accepted, session.rejected: the session owner and any signers in the
//     payload; for platform scope ops also get it by email and Slack.
//
// Each event is sent once: its id is claimed in Redis (in this process without Redis) before any
// send, and every email carries an idempotency key built from it. Logs never carry an address.
const (
	qaInviteTemplate          = "qa/invite"
	qaResultFailedTemplate    = "qa/result_failed"
	qaSessionClosedTemplate   = "qa/session_closed"
	qaSessionDecidedTemplate  = "qa/session_decided"
	qaOpsResultFailedTemplate = "platform/qa_result_failed"
	qaOpsSessionTemplate      = "platform/qa_session_update"

	qaKindInvite   = "invite.created"
	qaKindFailed   = "result.failed"
	qaKindClosed   = "session.closed"
	qaKindAccepted = "session.accepted"
	qaKindRejected = "session.rejected"

	qaScopePlatform = "platform"
	// qaSeenTTL is how long a handled event id is remembered; the projects stream keeps far less.
	qaSeenTTL       = 7 * 24 * time.Hour
	qaSeenKeyPrefix = "notif:project-qa:"
	// qaPlatformName names Codevertex as the inviter of a platform session.
	qaPlatformName = "Codevertex"
	// qaOpsReasonMax keeps a long decision reason (up to 2000 characters) out of the one-line
	// Slack and WhatsApp text; the email carries it whole.
	qaOpsReasonMax = 300
)

// qaEvent is the shared-events envelope projects-api publishes.
type qaEvent struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	EventType   string    `json:"event_type"`
	AggregateID string    `json:"aggregate_id"`
	Payload     qaPayload `json:"payload"`
}

// qaPayload holds the fields of every project.qa.* event (projects-api docs/api.md, Events).
type qaPayload struct {
	SessionID    string `json:"session_id"`
	TenantID     string `json:"tenant_id"`
	TenantSlug   string `json:"tenant_slug"`
	TenantName   string `json:"tenant_name"`
	SessionTitle string `json:"session_title"`
	Status       string `json:"status"`
	Scope        string `json:"scope"`
	SubjectKind  string `json:"subject_kind"`
	SubjectRef   string `json:"subject_ref"`
	OwnerID      string `json:"owner_id"`
	Link         string `json:"link"`

	// invite.created
	InviteID   string `json:"invite_id"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	InviteLink string `json:"invite_link"`
	// InvitedByName is read when projects-api starts sending it; the organisation names the
	// inviter until then.
	InvitedByName string `json:"invited_by_name"`

	// result.failed
	CriterionID    string `json:"criterion_id"`
	CriterionTitle string `json:"criterion_title"`
	AreaCode       string `json:"area_code"`
	RequirementRef string `json:"requirement_ref"`
	FeatureCode    string `json:"feature_code"`
	Severity       int    `json:"severity"`
	ResultID       string `json:"result_id"`
	DefectID       string `json:"defect_id"`
	TicketNumber   string `json:"ticket_number"`
	TicketStatus   string `json:"ticket_status"`
	TesterHint     string `json:"tester_hint"`
	RaisedByHand   bool   `json:"raised_by_hand"`

	// session.closed, session.accepted, session.rejected
	Accepted     *bool    `json:"accepted"`
	Score        *float64 `json:"score"`
	DocumentHash string   `json:"document_hash"`
	Reason       string   `json:"reason"`
	// Signers is read when projects-api starts sending the signers' addresses (it keeps only
	// hashes today); the owner gets the notice until then.
	Signers []qaPerson `json:"signers"`
}

func (p qaPayload) platform() bool { return strings.EqualFold(strings.TrimSpace(p.Scope), qaScopePlatform) }

// qaPerson is one email recipient.
type qaPerson struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// qaRoute says who an event goes to. It depends only on the event, so routing is testable
// without a database.
type qaRoute struct {
	// Platform sends Codevertex branded (platform sender scope) instead of the tenant's branding.
	Platform bool
	// Template is the email the invitee, the team or the owner and signers get ("" for none).
	Template string
	Invitee  bool
	// Team is the tenant's team: the session owner and the tenant's contact (admins as fallback).
	Team bool
	// OwnerSigners is the session owner and the signers in the payload.
	OwnerSigners bool
	Ops          bool
	OpsTemplate  string
	OpsWhatsApp  bool
}

// planQA routes one event. A tenant scope event (or one without a scope) never reaches ops.
func planQA(kind string, p qaPayload) (qaRoute, bool) {
	platform := p.platform()
	switch kind {
	case qaKindInvite:
		return qaRoute{Platform: platform, Template: qaInviteTemplate, Invitee: true}, true
	case qaKindFailed:
		if platform {
			return qaRoute{Platform: true, Ops: true, OpsTemplate: qaOpsResultFailedTemplate, OpsWhatsApp: p.Severity == 1}, true
		}
		return qaRoute{Template: qaResultFailedTemplate, Team: true}, true
	case qaKindClosed, qaKindAccepted, qaKindRejected:
		r := qaRoute{Platform: platform, Template: qaSessionDecidedTemplate, OwnerSigners: true}
		if kind == qaKindClosed {
			r.Template = qaSessionClosedTemplate
		}
		if platform {
			r.Ops, r.OpsTemplate = true, qaOpsSessionTemplate
		}
		return r, true
	}
	return qaRoute{}, false
}

// qaKind reads the event type from the subject (project.qa.<type>), else from the envelope
// (qa.<type>).
func qaKind(subject, eventType string) string {
	if k := strings.TrimPrefix(subject, "project.qa."); k != subject && k != "" {
		return k
	}
	return strings.TrimPrefix(eventType, "qa.")
}

// qaSeen remembers handled event ids when there is no Redis.
type qaSeen struct {
	mu  sync.Mutex
	mem map[string]time.Time
}

// claimQAEvent records key and reports whether this is its first delivery. A Redis error falls
// back to this process's memory, so a redelivery after a restart may send again; that beats
// dropping an invite.
func (s *Subscriber) claimQAEvent(ctx context.Context, key string) bool {
	if rdb := s.escrow.rdb; rdb != nil {
		ok, err := rdb.SetNX(ctx, qaSeenKeyPrefix+key, "1", qaSeenTTL).Result()
		if err == nil {
			return ok
		}
	}
	s.qaSeen.mu.Lock()
	defer s.qaSeen.mu.Unlock()
	if s.qaSeen.mem == nil {
		s.qaSeen.mem = map[string]time.Time{}
	}
	now := time.Now()
	if exp, ok := s.qaSeen.mem[key]; ok && now.Before(exp) {
		return false
	}
	s.qaSeen.mem[key] = now.Add(qaSeenTTL)
	return true
}

// qaEventKey is the event id, or the kind with the session and the item it is about when an older
// publisher sent no id.
func qaEventKey(e qaEvent, kind string) string {
	if id := strings.TrimSpace(e.ID); id != "" {
		return id
	}
	p := e.Payload
	return kind + ":" + firstNonEmpty(p.SessionID, e.AggregateID) + ":" + firstNonEmpty(p.InviteID, p.DefectID, p.Status)
}

func (s *Subscriber) handleProjectQA(msg *nats.Msg) {
	var e qaEvent
	if err := json.Unmarshal(msg.Data, &e); err != nil {
		s.log.Warn("project qa: unmarshal failed", zap.Error(err))
		_ = msg.Ack() // a malformed payload will never parse
		return
	}
	kind := qaKind(msg.Subject, e.EventType)
	route, ok := planQA(kind, e.Payload)
	if !ok {
		_ = msg.Ack()
		return
	}
	p := e.Payload
	tid := firstNonEmpty(p.TenantID, e.TenantID)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	key := qaEventKey(e, kind)
	if !s.claimQAEvent(ctx, key) {
		s.log.Info("project qa: event already handled", zap.String("event", kind), zap.String("session_id", p.SessionID))
		_ = msg.Ack()
		return
	}
	org := s.qaOrganisation(ctx, tid, p)
	data := qaData(kind, p, org)
	sent := 0
	idem := "project-qa-" + key

	if route.Invitee {
		if to := strings.TrimSpace(p.Email); strings.Contains(to, "@") {
			s.sendQAEmail(tid, route.Platform, route.Template, qaSubject(kind, p, org), qaPerson{Email: to, Name: p.Name}, data, idem)
			sent++
		}
	}
	if route.Team || route.OwnerSigners {
		var people []qaPerson
		if owner, ok := s.qaOwner(ctx, p.OwnerID); ok {
			people = append(people, owner)
		}
		if route.OwnerSigners {
			people = append(people, p.Signers...)
		}
		// The tenant's contact always hears of a failure; a decision goes to it only when neither the
		// owner nor a signer could be reached. Platform sessions are covered by ops instead.
		if !route.Platform && (route.Team || len(qaMergePeople(people)) == 0) {
			people = append(people, s.qaTenantTeam(ctx, tid, p.TenantSlug)...)
		}
		for _, to := range qaMergePeople(people) {
			s.sendQAEmail(tid, route.Platform, route.Template, qaSubject(kind, p, org), to, data, idem)
			sent++
		}
	}
	if route.Ops {
		s.sendOpsAlert(ctx, opsAlert{TenantID: tid, Template: route.OpsTemplate, Subject: qaOpsSubject(kind, p),
			Data: data, Text: qaOpsText(kind, p, data), From: "Codevertex QA", SlackPrefix: qaSlackPrefix(kind, p), WhatsApp: route.OpsWhatsApp})
	}
	s.log.Info("project qa notification dispatched", zap.String("event", kind), zap.String("session_id", p.SessionID),
		zap.String("tenant_id", tid), zap.Bool("platform", route.Platform), zap.Bool("ops", route.Ops), zap.Int("emails", sent))
	_ = msg.Ack()
}

// sendQAEmail queues one email. Platform scope sends from Codevertex; tenant scope from the
// tenant's own sender and branding.
func (s *Subscriber) sendQAEmail(tenantID string, platform bool, template, subject string, to qaPerson, base map[string]any, idem string) {
	data := make(map[string]any, len(base)+1)
	for k, v := range base {
		data[k] = v
	}
	data["name"] = firstNonEmpty(strings.TrimSpace(to.Name), "there")
	scope := messaging.SenderScopeTenant
	if platform {
		scope = messaging.SenderScopePlatform
	}
	msg := messaging.Message{
		TenantID:       tenantID,
		Channel:        "email",
		TemplateID:     template,
		SenderScope:    scope,
		Target:         messaging.TargetStaff,
		To:             []string{strings.TrimSpace(to.Email)},
		Data:           data,
		Metadata:       map[string]any{"subject": subject},
		IdempotencyKey: idem,
	}
	if err := s.publishMessage(msg); err != nil {
		s.log.Warn("project qa: email not queued", zap.String("template", template), zap.Error(err))
	}
}

// qaOwner reads the session owner's name and address from the local user table (synced from
// auth-api) by the auth user id.
func (s *Subscriber) qaOwner(ctx context.Context, ownerID string) (qaPerson, bool) {
	id, err := uuid.Parse(strings.TrimSpace(ownerID))
	if err != nil || id == uuid.Nil || s.entClient == nil {
		return qaPerson{}, false
	}
	u, err := s.entClient.User.Query().Where(entuser.AuthServiceUserIDEQ(id)).First(ctx)
	if err != nil || u == nil || strings.TrimSpace(u.Email) == "" {
		return qaPerson{}, false
	}
	return qaPerson{Email: u.Email, Name: u.FullName}, true
}

// qaTenantTeam is the tenant's contact address from auth-api, else its owner and admins from
// auth-api's reach list.
func (s *Subscriber) qaTenantTeam(ctx context.Context, tenantID, slug string) []qaPerson {
	if slug == "" {
		slug = s.qaTenantSlug(ctx, tenantID)
	}
	if d := s.fetchTenantDetail(slug); d != nil && strings.Contains(d.ContactEmail, "@") {
		return []qaPerson{{Email: d.ContactEmail, Name: d.Name}}
	}
	if tenantID == "" {
		return nil
	}
	var out []qaPerson
	for _, p := range s.tenantAdmins(ctx, tenantID) {
		for _, e := range p.Emails {
			out = append(out, qaPerson{Email: e.Value, Name: firstNonEmpty(e.FirstName, p.BusinessName)})
		}
	}
	return out
}

// qaTenantSlug reads a tenant's slug from the local tenant table.
func (s *Subscriber) qaTenantSlug(ctx context.Context, tenantID string) string {
	id, err := uuid.Parse(strings.TrimSpace(tenantID))
	if err != nil || s.entClient == nil {
		return ""
	}
	t, err := s.entClient.Tenant.Get(ctx, id)
	if err != nil || t == nil {
		return ""
	}
	return t.Slug
}

// qaOrganisation names who runs the session: Codevertex for a platform session, else the tenant's
// name from the event or the local tenant table. Never a bare id.
func (s *Subscriber) qaOrganisation(ctx context.Context, tenantID string, p qaPayload) string {
	if p.platform() {
		return qaPlatformName
	}
	if n := strings.TrimSpace(p.TenantName); n != "" {
		return n
	}
	if l := s.tenantLabel(ctx, tenantID); l != "" && l != tenantID {
		return l
	}
	return strings.TrimSpace(p.TenantSlug)
}

// qaMergePeople drops blank addresses and repeats (case-insensitive), keeping the first name seen.
func qaMergePeople(in []qaPerson) []qaPerson {
	seen := map[string]bool{}
	var out []qaPerson
	for _, p := range in {
		addr := strings.TrimSpace(p.Email)
		k := strings.ToLower(addr)
		if !strings.Contains(addr, "@") || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, qaPerson{Email: addr, Name: strings.TrimSpace(p.Name)})
	}
	return out
}

// qaData builds the email data. Optional values are left out when empty so the templates'
// {{ if }} guards drop the line.
func qaData(kind string, p qaPayload, org string) map[string]any {
	d := map[string]any{
		"session_title": firstNonEmpty(strings.TrimSpace(p.SessionTitle), "a QA session"),
		"platform":      p.platform(),
	}
	put := func(k, v string) {
		if v = strings.TrimSpace(v); v != "" {
			d[k] = v
		}
	}
	put("organisation", org)
	put("subject", qaSubjectLabel(p.SubjectKind, p.SubjectRef))
	put("link", p.Link)
	switch kind {
	case qaKindInvite:
		put("invite_link", p.InviteLink)
		put("invited_by", p.InvitedByName)
		role := strings.ToLower(strings.TrimSpace(p.Role))
		if role == "" {
			role = "tester"
		}
		d["role"] = role
		d["role_task"] = qaRoleTask(role)
	case qaKindFailed:
		put("criterion_title", firstNonEmpty(p.CriterionTitle, "A criterion"))
		put("area_code", p.AreaCode)
		put("requirement_ref", p.RequirementRef)
		put("feature_code", p.FeatureCode)
		put("ticket_number", p.TicketNumber)
		put("ticket_status", qaTicketStatus(p.TicketStatus, p.TicketNumber))
		put("tester_hint", p.TesterHint)
		put("defect_id", p.DefectID)
		put("result_id", p.ResultID)
		if p.Severity > 0 {
			d["severity"] = p.Severity
			d["severity_label"] = qaSeverityLabel(p.Severity)
		}
		d["raised_by_hand"] = p.RaisedByHand
	case qaKindClosed:
		d["state"] = "closed"
		if p.Accepted != nil {
			d["outcome_known"] = true
			d["passed"] = *p.Accepted
		}
		if p.Score != nil {
			d["score"] = strconv.FormatFloat(*p.Score, 'f', -1, 64)
		}
		put("document_hash", p.DocumentHash)
	case qaKindAccepted, qaKindRejected:
		d["accepted"] = kind == qaKindAccepted
		d["state"] = strings.TrimPrefix(kind, "session.")
		put("reason", p.Reason)
		put("document_hash", p.DocumentHash)
	}
	return d
}

// qaSubjectLabel words what the session tests ("Service hadia", "Project"); a general session
// has none.
func qaSubjectLabel(kind, ref string) string {
	kind, ref = strings.TrimSpace(kind), strings.TrimSpace(ref)
	switch kind {
	case "service":
		if ref == "" {
			return "A Codevertex service"
		}
		return "Service " + ref
	case "project":
		return "Project"
	}
	return ""
}

func qaRoleTask(role string) string {
	switch role {
	case "signer":
		return "review the results and sign the acceptance"
	case "observer":
		return "follow the session and its results"
	}
	return "test each criterion and record what you find"
}

func qaSeverityLabel(n int) string {
	switch n {
	case 1:
		return "1 (critical)"
	case 2:
		return "2 (major)"
	case 3:
		return "3 (minor)"
	case 4:
		return "4 (cosmetic)"
	}
	return strconv.Itoa(n)
}

// qaTicketStatus words the defect's ticket state.
func qaTicketStatus(status, number string) string {
	switch status {
	case "ticketed":
		if number != "" {
			return "Ticket " + number + " opened"
		}
		return "Ticket opened"
	case "ticket_failed":
		return "The ticket could not be opened yet; raise it from the session"
	case "open":
		return "No ticket yet"
	}
	return status
}

// qaSubject is the subject of the invitee, team and owner emails.
func qaSubject(kind string, p qaPayload, org string) string {
	title := firstNonEmpty(strings.TrimSpace(p.SessionTitle), "a QA session")
	switch kind {
	case qaKindInvite:
		if org != "" {
			return org + " invited you to " + title
		}
		return "You are invited to " + title
	case qaKindFailed:
		if p.Severity > 0 {
			return fmt.Sprintf("Severity %d failure in %s: %s", p.Severity, title, firstNonEmpty(p.CriterionTitle, "a criterion"))
		}
		return "Failure in " + title + ": " + firstNonEmpty(p.CriterionTitle, "a criterion")
	case qaKindClosed:
		return title + " is closed"
	case qaKindAccepted:
		return title + " was accepted"
	case qaKindRejected:
		return title + " was rejected"
	}
	return title
}

func qaOpsSubject(kind string, p qaPayload) string {
	if kind == qaKindFailed {
		return "QA " + qaSubject(kind, p, "")
	}
	return "QA: " + qaSubject(kind, p, "")
}

func qaSlackPrefix(kind string, p qaPayload) string {
	switch {
	case kind == qaKindFailed && p.Severity == 1:
		return ":rotating_light: "
	case kind == qaKindFailed:
		return ":warning: "
	case kind == qaKindAccepted:
		return ":white_check_mark: "
	case kind == qaKindRejected:
		return ":x: "
	}
	return ":clipboard: "
}

// qaOpsText is the one line for Slack and WhatsApp (Meta refuses a template parameter with a line
// break). It names no person: the tester hint stays in the email.
func qaOpsText(kind string, p qaPayload, d map[string]any) string {
	title := d["session_title"].(string)
	where := title
	if sub, ok := d["subject"].(string); ok {
		where += " (" + sub + ")"
	}
	var b strings.Builder
	switch kind {
	case qaKindFailed:
		b.WriteString("QA failure in " + where + ": " + d["criterion_title"].(string))
		if l, ok := d["severity_label"].(string); ok {
			b.WriteString(", severity " + l)
		}
		if a, ok := d["area_code"].(string); ok {
			b.WriteString(", area " + a)
		}
		if f, ok := d["feature_code"].(string); ok {
			b.WriteString(", feature " + f)
		}
		b.WriteString(".")
		if t, ok := d["ticket_status"].(string); ok {
			b.WriteString(" " + t + ".")
		}
	case qaKindClosed:
		b.WriteString("QA session closed: " + where + ".")
		if known, _ := d["outcome_known"].(bool); known {
			if d["passed"].(bool) {
				b.WriteString(" The pass rule is met")
			} else {
				b.WriteString(" The pass rule is not met")
			}
			if sc, ok := d["score"].(string); ok {
				b.WriteString(", score " + sc)
			}
			b.WriteString(".")
		}
	case qaKindAccepted, qaKindRejected:
		verb := "accepted"
		if kind == qaKindRejected {
			verb = "rejected"
		}
		b.WriteString("QA session " + verb + ": " + where + ".")
		if r, ok := d["reason"].(string); ok {
			r = strings.Join(strings.Fields(r), " ")
			if rs := []rune(r); len(rs) > qaOpsReasonMax {
				r = string(rs[:qaOpsReasonMax]) + "..."
			}
			b.WriteString(" Reason: " + r)
		}
	}
	if l, ok := d["link"].(string); ok {
		b.WriteString(" " + l)
	}
	return b.String()
}
