package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/modules/preferences"
)

// One notification, several parties, several channels.
//
// A consumer that tells more than one person about the same thing (a customer and the business,
// a rider and the business, every responder on an incident) builds one partyNotice and calls
// sendToParties. Each party gets the channels it has a contact for, out of the channels it allows
// (email, sms, whatsapp, push). The rules live here once: placeholder emails are skipped, a
// WhatsApp button template never goes without its link, phone channels get the tenant's dial code,
// idempotency keys are per notice, channel and party, and, when the caller passes a Gate, the
// tenant's channel preferences and drafted templates are applied (fanOutTargets).
//
// Active channels: email and WhatsApp (feedback_active_channels_email_whatsapp). SMS and push are
// supported for callers that need them but are never on by default.

// defaultPartyChannels are the channels a party gets when it names none.
var defaultPartyChannels = []string{"email", "whatsapp"}

// partyWhatsApp is the approved WhatsApp template for one party.
type partyWhatsApp struct {
	Name   string
	Params []string // body parameters in {{1}}..{{n}} order
	// Button is the URL suffix of a _btn template (empty: WhatsApp is skipped for this party), or
	// the value of a non-link button such as an AUTHENTICATION code.
	Button string
}

// noticeParty is one recipient of a notice.
type noticeParty struct {
	// Key tells parties of the same notice apart in the idempotency key (empty for a notice
	// with a single recipient).
	Key    string
	Target string // messaging.Target*
	Email  string
	Phone  string // sms and whatsapp
	PushTo string // push recipient (user id), when the caller sends push
	// Channels this party may get (nil: defaultPartyChannels).
	Channels []string
	// Per-party overrides of the notice's template, subject and data.
	TemplateID   string
	WATemplateID string
	Subject      string
	Data         map[string]any
	WhatsApp     *partyWhatsApp
}

// partyNotice is one notification for several parties.
type partyNotice struct {
	ServiceID  string
	TenantID   string
	TemplateID string // templates/<channel>/<id>
	// WATemplateID is the WhatsApp text file when it differs from TemplateID.
	WATemplateID string
	Subject      string
	Scope        string // messaging.SenderScope* (default tenant)
	Data         map[string]any
	Country      string // tenant ISO country for phone dial codes
	// IdemBase prefixes every idempotency key: <IdemBase>-<channel><party key suffix>.
	IdemBase string
	Parties  []noticeParty
	// Gate and TemplateChannels, when set, keep only channels the tenant enabled for this
	// template and that have a drafted template (as order and treasury notices do).
	Gate             *preferences.Gate
	TemplateChannels map[string][]string
}

// sendableEmail rejects empty, malformed and placeholder (.local, .invalid) addresses.
func sendableEmail(email string) bool {
	return email != "" && strings.Contains(email, "@") &&
		!strings.HasSuffix(strings.ToLower(email), ".local") && !strings.HasSuffix(strings.ToLower(email), ".invalid")
}

// partyIdemSuffix is the key suffix for a party: none for a single unnamed party, so a notice
// with one recipient keeps the plain <base>-<channel> key.
func partyIdemSuffix(n partyNotice, p noticeParty) string {
	if p.Key == "" {
		return ""
	}
	return "-" + p.Key
}

// sendToParties publishes every party's messages. All parties are attempted; the first publish
// error is returned so the caller can redeliver the event (sent channels are deduplicated by
// their idempotency keys).
func sendToParties(ctx context.Context, nc *nats.Conn, cfg *config.Config, n partyNotice) error {
	var firstErr error
	for _, p := range n.Parties {
		if err := sendToParty(ctx, nc, cfg, n, p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func sendToParty(ctx context.Context, nc *nats.Conn, cfg *config.Config, n partyNotice, p noticeParty) error {
	templateID := n.TemplateID
	if p.TemplateID != "" {
		templateID = p.TemplateID
	}
	waTemplateID := n.WATemplateID
	if p.WATemplateID != "" {
		waTemplateID = p.WATemplateID
	}
	subject := n.Subject
	if p.Subject != "" {
		subject = p.Subject
	}
	data := make(map[string]any, len(n.Data)+len(p.Data)+1)
	for k, v := range n.Data {
		data[k] = v
	}
	for k, v := range p.Data {
		data[k] = v
	}
	scope := n.Scope
	if scope == "" {
		scope = messaging.SenderScopeTenant
	}

	contacts := partyContacts(p)
	if n.Gate != nil || n.TemplateChannels != nil {
		contacts = fanOutTargets(ctx, n.Gate, n.TemplateChannels, n.TenantID, templateID, contacts)
	}
	return publishPartyMessages(ctx, nc, cfg, n, p, contacts, templateID, waTemplateID, subject, scope, data)
}

// partyContacts maps each channel the party allows and can be reached on to its address.
func partyContacts(p noticeParty) map[string]string {
	allowed := p.Channels
	if allowed == nil {
		allowed = defaultPartyChannels
	}
	contacts := map[string]string{}
	for _, ch := range allowed {
		switch ch {
		case "email":
			if sendableEmail(p.Email) {
				contacts["email"] = p.Email
			}
		case "sms":
			if p.Phone != "" {
				contacts["sms"] = p.Phone
			}
		case "push":
			if p.PushTo != "" {
				contacts["push"] = p.PushTo
			}
		case "whatsapp":
			if p.Phone == "" || p.WhatsApp == nil || p.WhatsApp.Name == "" {
				continue
			}
			if strings.HasSuffix(p.WhatsApp.Name, "_btn") && p.WhatsApp.Button == "" {
				continue // a button template cannot go without its link
			}
			contacts["whatsapp"] = p.Phone
		}
	}
	return contacts
}

// publishPartyMessages publishes one message per resolved channel of a party.
func publishPartyMessages(ctx context.Context, nc *nats.Conn, cfg *config.Config, n partyNotice, p noticeParty,
	contacts map[string]string, templateID, waTemplateID, subject, scope string, data map[string]any) error {
	dial := dialCodeForCountry(n.Country)
	if dial == "" {
		dial = "254"
	}
	var firstErr error
	for _, ch := range channelOrder {
		to, ok := contacts[ch]
		if !ok {
			continue
		}
		meta := map[string]any{"service_id": n.ServiceID}
		tpl := templateID
		switch ch {
		case "email":
			meta["subject"] = subject
		case "sms":
			meta["default_dial_code"] = dial
		case "whatsapp":
			if waTemplateID != "" {
				tpl = waTemplateID
			}
			meta["template_name"] = p.WhatsApp.Name
			meta["template_language"] = "en_US"
			meta["template_params"] = p.WhatsApp.Params
			if p.WhatsApp.Button != "" {
				meta["template_button_param"] = p.WhatsApp.Button
			}
			meta["default_dial_code"] = dial
		}
		if _, err := messaging.Publish(ctx, nc, cfg.Events, messaging.Message{
			TenantID:       n.TenantID,
			Channel:        ch,
			TemplateID:     tpl,
			SenderScope:    scope,
			Target:         p.Target,
			To:             []string{to},
			Data:           data,
			Metadata:       meta,
			RequestID:      uuid.New().String(),
			IdempotencyKey: fmt.Sprintf("%s-%s%s", n.IdemBase, ch, partyIdemSuffix(n, p)),
			QueuedAt:       time.Now(),
		}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
