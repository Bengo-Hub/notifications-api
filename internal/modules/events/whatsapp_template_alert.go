package events

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/whatsapp/templatecat"
)

// whatsappTemplateChangeTemplate is the ops email for a WhatsApp template Meta moved, rejected,
// paused or disabled.
const whatsappTemplateChangeTemplate = "platform/whatsapp_template_change"

// WhatsAppTemplateChange is one change Meta made to one of our templates (templatecat.Change).
type WhatsAppTemplateChange = templatecat.Change

func titleCategory(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if c == "" {
		return "an unknown category"
	}
	return strings.ToUpper(c[:1]) + c[1:]
}

// whatsAppTemplateAlert words the alert. email is false for news that needs no action (an
// approval, or a move to a cheaper category), which goes to Slack only.
func whatsAppTemplateAlert(c WhatsAppTemplateChange) (subject, text string, data map[string]any, email bool) {
	from, to := titleCategory(c.PreviousCategory), titleCategory(c.NewCategory)
	data = map[string]any{
		"template": c.Name, "language": c.Language, "waba_id": c.WABAID, "source": c.Source,
		"previous_category": from, "new_category": to, "status": c.Status, "reason": c.Reason,
	}
	switch {
	case c.Kind == "category" && strings.EqualFold(c.NewCategory, "MARKETING"):
		subject = "WhatsApp template moved to Marketing: " + c.Name
		data["headline"] = "Meta moved a WhatsApp template to Marketing"
		data["impact"] = "Meta now bills every send of this template at the marketing rate, about five times utility, " +
			"and stops delivering it to a person after the first one or two marketing messages (error 131049 in Monitoring). " +
			"Credits now charge tenants for it as marketing."
		data["action"] = "Rewrite it under a new _vN name as a plain fact about the reader's own account, order or request, " +
			"with no invitation to buy, renew, share or visit, and map the new name to this one in superseded.go so sends " +
			"keep going until Meta approves it. If the message really is promotional, keep it and record " +
			"\"meta_category\": \"MARKETING\" in templates.json. Rules: docs/whatsapp-template-policy.md section 3b."
		email = true
		text = fmt.Sprintf(":rotating_light: *Meta moved WhatsApp template `%s` from %s to Marketing* (WABA %s).\n"+
			"Each send now costs about 5x and stops reaching people after their first marketing message or two (error 131049). "+
			"Credits now charge it as marketing.\n*Action:* rewrite it as a utility `_vN` (policy 3b) or record `meta_category`.",
			c.Name, from, c.WABAID)
	case c.Kind == "category":
		subject = fmt.Sprintf("WhatsApp template now %s: %s", to, c.Name)
		data["headline"] = "Meta changed a WhatsApp template's category"
		data["impact"] = fmt.Sprintf("Meta now bills it as %s instead of %s, and credits charge it the same way.", to, from)
		data["action"] = "Nothing to do unless the new category is wrong for the message."
		email = categoryPrice(c.NewCategory) > categoryPrice(c.PreviousCategory)
		text = fmt.Sprintf(":information_source: Meta moved WhatsApp template `%s` from %s to %s (WABA %s). Credits follow.",
			c.Name, from, to, c.WABAID)
	case strings.EqualFold(c.Status, "APPROVED"):
		subject = "WhatsApp template approved: " + c.Name
		data["headline"] = "Meta approved a WhatsApp template"
		data["impact"] = "Sends that use it now go out with this wording."
		data["action"] = "If it replaces an older template, remove its entry from superseded.go."
		text = fmt.Sprintf(":white_check_mark: Meta approved WhatsApp template `%s` (WABA %s).", c.Name, c.WABAID)
	default:
		status := strings.ToUpper(c.Status)
		subject = fmt.Sprintf("WhatsApp template %s: %s", strings.ToLower(status), c.Name)
		data["headline"] = "Meta stopped a WhatsApp template: " + strings.ToLower(status)
		data["impact"] = "Sends that use it fail, or fall back to an older approved template where superseded.go maps one. " +
			"The email copy of each notice still goes."
		data["action"] = "Read Meta's reason, fix the wording under a new _vN name and submit it from Templates, Sync."
		email = true
		reason := ""
		if c.Reason != "" && !strings.EqualFold(c.Reason, "NONE") {
			reason = " Reason: " + c.Reason + "."
		}
		text = fmt.Sprintf(":x: *Meta set WhatsApp template `%s` to %s* (WABA %s).%s Sends using it fail or fall back to the older template.",
			c.Name, status, c.WABAID, reason)
	}
	return subject, text, data, email
}

func categoryPrice(cat string) int {
	switch strings.ToUpper(cat) {
	case "MARKETING":
		return 3
	case "UTILITY":
		return 2
	case "AUTHENTICATION":
		return 1
	}
	return 0
}

// AlertWhatsAppTemplateChange tells ops about a change Meta made to one of our WhatsApp templates,
// by email and Slack. WhatsApp is never used for it (ops WhatsApp alerts are off by default, owner
// 16 November 2026), and news that needs no action goes to Slack only.
func (s *Subscriber) AlertWhatsAppTemplateChange(ctx context.Context, c WhatsAppTemplateChange) {
	if s == nil {
		return
	}
	subject, text, data, email := whatsAppTemplateAlert(c)
	if !email {
		if hook := s.platformAlertSettings(ctx)["slack_webhook_url"]; hook != "" {
			s.postSlack(ctx, hook, text)
		}
	} else {
		s.sendOpsAlert(ctx, opsAlert{Template: whatsappTemplateChangeTemplate, Subject: subject, Data: data, Text: text})
	}
	s.log.Info("whatsapp template change alert", zap.String("template", c.Name), zap.String("kind", c.Kind),
		zap.String("new_category", c.NewCategory), zap.String("status", c.Status), zap.String("source", c.Source), zap.Bool("email", email))
}
