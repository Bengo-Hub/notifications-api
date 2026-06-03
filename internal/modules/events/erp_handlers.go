package events

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/messaging"
)

// genericEmailTemplate is the passthrough template used when the ERP sends a raw
// email (subject + message) without referencing a named template. It renders the
// caller-supplied title/message inside the shared branded email shell.
const genericEmailTemplate = "shared/generic_notification"

// erpAttachment mirrors a single attachment in the erp.email.requested payload.
// The current send pipeline (messaging.Message -> worker -> EmailProvider.SendEmail)
// has no field for inline attachments, so these are logged and dropped — see
// handleERPEmailRequested.
type erpAttachment struct {
	Filename string `json:"filename"`
	Content  string `json:"content"`
	Mimetype string `json:"mimetype"`
	Encoding string `json:"encoding"`
}

// erpEmailPayload is the payload of an erp.email.requested event.
type erpEmailPayload struct {
	Subject       string          `json:"subject"`
	Message       string          `json:"message"`
	RecipientList []string        `json:"recipient_list"`
	HTMLMessage   *string         `json:"html_message"`
	FromEmail     *string         `json:"from_email"`
	CC            []string        `json:"cc"`
	BCC           []string        `json:"bcc"`
	ReplyTo       *string         `json:"reply_to"`
	Attachments   []erpAttachment `json:"attachments"`
}

// erpNotificationPayload is the payload of an erp.notification.requested event.
type erpNotificationPayload struct {
	UserID           *string        `json:"user_id"`
	Email            *string        `json:"email"`
	Title            string         `json:"title"`
	Message          string         `json:"message"`
	NotificationType string         `json:"notification_type"`
	Channels         []string       `json:"channels"`
	Data             map[string]any `json:"data"`
	ImageURL         *string        `json:"image_url"`
	ActionURL        *string        `json:"action_url"`
	EmailSubject     *string        `json:"email_subject"`
	Templates        struct {
		Email *string `json:"email"`
		SMS   *string `json:"sms"`
		Push  *string `json:"push"`
	} `json:"templates"`
	Context map[string]any `json:"context"`
}

// erpEnvelope is the standard outbox envelope wrapping ERP events. Only the
// fields the handlers need are decoded; the raw payload is decoded per-event.
type erpEnvelope struct {
	ID        string          `json:"id"`
	TenantID  string          `json:"tenant_id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

// publishMessage enqueues a fully-formed messaging.Message onto the
// notifications.events stream — the same pipeline the REST handler and the other
// cross-service consumers use. The worker process renders the template and
// delivers via the configured provider.
func (s *Subscriber) publishMessage(msg messaging.Message) error {
	if msg.RequestID == "" {
		msg.RequestID = uuid.New().String()
	}
	if msg.QueuedAt.IsZero() {
		msg.QueuedAt = time.Now()
	}
	_, err := messaging.Publish(context.Background(), s.nc, s.cfg, msg)
	return err
}

// handleERPEmailRequested handles erp.email.requested: a raw transactional email
// (subject + message, optional cc) routed through the SAME messaging.Message ->
// worker -> EmailProvider.SendEmail pipeline the REST POST .../notifications/messages
// email path uses. The raw body is rendered via the shared generic_notification
// template (the pipeline always renders through a template; there is no raw-body
// passthrough).
//
// attachments ARE supported: base64 content is decoded and carried through
// messaging.Message.Attachments to every email provider (SMTP/Brevo/SendGrid).
// Remaining limitations (logged, not carried by messaging.Message / the provider API):
//   - bcc, reply_to, from_email — SendEmail signature is (from, to, cc, subject, html, text, attachments)
//     and `from` is only used as a display-name override, not a real From address
//   - html_message              — the generic template HTML-escapes .message, so a
//     pre-rendered HTML body cannot be injected safely; the plain message is sent
//
// cc IS supported (messaging.Message.Cc). Plan-based email rate limiting is enforced
// only at the HTTP entrypoint (it needs JWT plan claims); like every other
// cross-service consumer, async events are not rate-limited here.
func (s *Subscriber) handleERPEmailRequested(msg *nats.Msg) {
	var env erpEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		s.log.Warn("erp.email.requested: envelope unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	var p erpEmailPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.log.Warn("erp.email.requested: payload unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}

	if env.TenantID == "" {
		s.log.Warn("erp.email.requested: missing tenant_id, dropping", zap.String("event_id", env.ID))
		_ = msg.Ack()
		return
	}

	to := messaging.NormalizeRecipients(p.RecipientList)
	if len(to) == 0 {
		s.log.Warn("erp.email.requested: no valid recipients, dropping",
			zap.String("tenant_id", env.TenantID), zap.String("event_id", env.ID))
		_ = msg.Ack()
		return
	}
	cc := messaging.NormalizeRecipients(p.CC)

	subject := strings.TrimSpace(p.Subject)
	if subject == "" {
		subject = "Notification"
	}

	// Decode optional attachments (base64) — carried through to the email provider.
	var atts []messaging.Attachment
	for _, a := range p.Attachments {
		if a.Filename == "" || a.Content == "" {
			continue
		}
		raw, derr := base64.StdEncoding.DecodeString(a.Content)
		if derr != nil {
			s.log.Warn("erp.email.requested: skipping attachment with invalid base64",
				zap.String("tenant_id", env.TenantID), zap.String("filename", a.Filename))
			continue
		}
		atts = append(atts, messaging.Attachment{Filename: a.Filename, ContentType: a.Mimetype, Content: raw})
	}

	if p.HTMLMessage != nil && *p.HTMLMessage != "" {
		s.log.Warn("erp.email.requested: html_message ignored — pipeline renders plain message through generic template",
			zap.String("tenant_id", env.TenantID))
	}
	if (p.FromEmail != nil && *p.FromEmail != "") ||
		len(p.BCC) > 0 ||
		(p.ReplyTo != nil && *p.ReplyTo != "") {
		s.log.Warn("erp.email.requested: from_email/bcc/reply_to dropped — not supported by EmailProvider interface",
			zap.String("tenant_id", env.TenantID))
	}

	m := messaging.Message{
		TenantID:    env.TenantID,
		Channel:     "email",
		TemplateID:  genericEmailTemplate,
		SenderScope: messaging.SenderScopeTenant,
		Target:      messaging.TargetStaff,
		To:          to,
		Cc:          cc,
		Attachments: atts,
		Data: map[string]any{
			"title":   subject,
			"message": p.Message,
		},
		Metadata: map[string]any{
			"subject": subject,
		},
		IdempotencyKey: erpIdempotencyKey("erp-email", env.ID, "email"),
	}

	if err := s.publishMessage(m); err != nil {
		s.log.Warn("erp.email.requested: publish failed (will retry)",
			zap.String("tenant_id", env.TenantID), zap.Error(err))
		_ = msg.Nak()
		return
	}

	s.log.Info("erp.email.requested dispatched",
		zap.String("tenant_id", env.TenantID),
		zap.Int("recipients", len(to)),
		zap.String("event_id", env.ID),
	)
	_ = msg.Ack()
}

// handleERPNotificationRequested handles erp.notification.requested: a multi-channel
// notification fanned out across the requested channels (email/sms/push). Each
// supported channel is published as its own messaging.Message through the existing
// pipeline. A template is used when templates.<channel> or notification_type is
// provided; otherwise the shared generic_notification template (title + message) is
// used for email.
//
// Recipient resolution: email/push use the payload `email`; in_app is acknowledged
// but skipped (the delivery pipeline has no in_app provider); sms is skipped because
// the payload carries no phone number and the SMS provider requires one. These gaps
// are logged, not silently ignored.
func (s *Subscriber) handleERPNotificationRequested(msg *nats.Msg) {
	var env erpEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		s.log.Warn("erp.notification.requested: envelope unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}
	var p erpNotificationPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		s.log.Warn("erp.notification.requested: payload unmarshal failed", zap.Error(err))
		_ = msg.Nak()
		return
	}

	if env.TenantID == "" {
		s.log.Warn("erp.notification.requested: missing tenant_id, dropping", zap.String("event_id", env.ID))
		_ = msg.Ack()
		return
	}

	channels := p.Channels
	if len(channels) == 0 {
		channels = []string{"email"}
	}

	email := ""
	if p.Email != nil {
		email = strings.TrimSpace(*p.Email)
	}

	// Shared template data — used by both named and generic templates.
	data := map[string]any{
		"title":   p.Title,
		"message": p.Message,
	}
	for k, v := range p.Data {
		data[k] = v
	}
	if p.ActionURL != nil && *p.ActionURL != "" {
		data["action_link"] = *p.ActionURL
	}
	if p.ImageURL != nil && *p.ImageURL != "" {
		data["image_url"] = *p.ImageURL
	}

	emailSubject := p.Title
	if p.EmailSubject != nil && *p.EmailSubject != "" {
		emailSubject = *p.EmailSubject
	}
	if emailSubject == "" {
		emailSubject = "Notification"
	}

	published := 0
	var publishErr error
	for _, raw := range channels {
		ch := strings.ToLower(strings.TrimSpace(raw))
		switch ch {
		case "email":
			recips := messaging.NormalizeRecipients([]string{email})
			if len(recips) == 0 {
				s.log.Warn("erp.notification.requested: email channel requested but no valid email, skipping",
					zap.String("tenant_id", env.TenantID), zap.String("event_id", env.ID))
				continue
			}
			m := messaging.Message{
				TenantID:    env.TenantID,
				Channel:     "email",
				TemplateID:  resolveTemplate(p.Templates.Email, p.NotificationType, "email", genericEmailTemplate),
				SenderScope: messaging.SenderScopeTenant,
				Target:      messaging.TargetCustomer,
				To:          recips,
				Data:        data,
				Metadata:    map[string]any{"subject": emailSubject},
				IdempotencyKey: erpIdempotencyKey("erp-notif", env.ID, "email"),
			}
			if err := s.publishMessage(m); err != nil {
				publishErr = err
				s.log.Warn("erp.notification.requested: email publish failed",
					zap.String("tenant_id", env.TenantID), zap.Error(err))
				continue
			}
			published++

		case "push":
			recips := messaging.NormalizeRecipients([]string{email})
			if len(recips) == 0 {
				s.log.Warn("erp.notification.requested: push channel requested but no recipient token/email, skipping",
					zap.String("tenant_id", env.TenantID), zap.String("event_id", env.ID))
				continue
			}
			tmpl := resolveTemplate(p.Templates.Push, p.NotificationType, "push", "")
			if tmpl == "" {
				s.log.Warn("erp.notification.requested: push channel has no template (no generic push template exists), skipping",
					zap.String("tenant_id", env.TenantID))
				continue
			}
			m := messaging.Message{
				TenantID:    env.TenantID,
				Channel:     "push",
				TemplateID:  tmpl,
				SenderScope: messaging.SenderScopeTenant,
				Target:      messaging.TargetCustomer,
				To:          recips,
				Data:        data,
				Metadata:    map[string]any{"push_title": p.Title},
				IdempotencyKey: erpIdempotencyKey("erp-notif", env.ID, "push"),
			}
			if err := s.publishMessage(m); err != nil {
				publishErr = err
				s.log.Warn("erp.notification.requested: push publish failed",
					zap.String("tenant_id", env.TenantID), zap.Error(err))
				continue
			}
			published++

		case "sms":
			// The ERP notification payload has no phone number and the SMS provider
			// requires E.164 recipients, so SMS cannot be delivered from this event.
			s.log.Warn("erp.notification.requested: sms channel skipped — payload has no phone number",
				zap.String("tenant_id", env.TenantID), zap.String("event_id", env.ID))

		case "in_app", "inapp":
			// No in_app delivery backend exists in the worker pipeline.
			s.log.Warn("erp.notification.requested: in_app channel skipped — no in_app delivery backend",
				zap.String("tenant_id", env.TenantID), zap.String("event_id", env.ID))

		default:
			s.log.Warn("erp.notification.requested: unknown channel skipped",
				zap.String("channel", ch), zap.String("tenant_id", env.TenantID))
		}
	}

	// Transient publish failure with nothing delivered → Nak for redelivery.
	if published == 0 && publishErr != nil {
		_ = msg.Nak()
		return
	}

	s.log.Info("erp.notification.requested dispatched",
		zap.String("tenant_id", env.TenantID),
		zap.Int("channels_sent", published),
		zap.String("notification_type", p.NotificationType),
		zap.String("event_id", env.ID),
	)
	_ = msg.Ack()
}

// resolveTemplate picks the template id for a channel: an explicit template wins,
// then "<channel>/<notification_type>" if a notification_type is given, else the
// supplied fallback (which may be "" when no generic template exists for the channel).
func resolveTemplate(explicit *string, notificationType, channel, fallback string) string {
	if explicit != nil && strings.TrimSpace(*explicit) != "" {
		return strings.TrimSpace(*explicit)
	}
	if nt := strings.TrimSpace(notificationType); nt != "" {
		return channel + "/" + nt
	}
	return fallback
}

// erpIdempotencyKey builds a stable idempotency key from the outbox event id so the
// downstream worker dedupes redeliveries per channel. Falls back to a random id when
// the envelope carries no id.
func erpIdempotencyKey(prefix, eventID, channel string) string {
	if eventID == "" {
		return fmt.Sprintf("%s-%s-%s", prefix, channel, uuid.New().String())
	}
	return fmt.Sprintf("%s-%s-%s", prefix, eventID, channel)
}
