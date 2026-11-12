package credits

import (
	"regexp"
	"strings"
	"sync"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

// WhatsApp is billed per Meta pricing category (owner decision 2026-11-12): each send spends
// only from its own category's credits in subscriptions-api. Prices live there
// (whatsapp_categories.json); this table only maps a Meta category to its credit service and the
// plain name tenants read. Keep names in step with subscriptions-api.

// WhatsApp category credit services in subscriptions-api.
const (
	ServiceWhatsAppMarketing      = "whatsapp_marketing"
	ServiceWhatsAppUtility        = "whatsapp_utility"
	ServiceWhatsAppAuthentication = "whatsapp_authentication"
	ServiceWhatsAppService        = "whatsapp_service"
)

// WhatsAppCategory is one Meta category and the credits that pay for it.
type WhatsAppCategory struct {
	Service      string
	MetaCategory string
	Name         string
	Explainer    string
	// Examples are shown when the tenant has sent nothing in the category yet.
	Examples []string
}

var whatsAppCategories = []WhatsAppCategory{
	{
		Service: ServiceWhatsAppMarketing, MetaCategory: "MARKETING", Name: "Promotions and broadcasts",
		Explainer: "Messages you send to many customers at once to promote your business.",
		Examples:  []string{"Holiday and occasion greetings", "Offers and announcements you broadcast"},
	},
	{
		Service: ServiceWhatsAppUtility, MetaCategory: "UTILITY", Name: "Order, payment and account updates",
		Explainer: "Messages about something a customer did or asked for.",
		Examples:  []string{"Order confirmed and delivered", "Payment receipts and invoices", "Payment reminders", "Account notices"},
	},
	{
		Service: ServiceWhatsAppAuthentication, MetaCategory: "AUTHENTICATION", Name: "Login and verification codes",
		Explainer: "One-time codes that let a person sign in or reset a password.",
		Examples:  []string{"Sign-in codes", "Password reset codes"},
	},
	{
		Service: ServiceWhatsAppService, MetaCategory: "SERVICE", Name: "Replies to customers",
		Explainer: "Messages sent in reply to a customer who wrote to you in the last 24 hours.",
		Examples:  []string{"Replies you type in the WhatsApp inbox", "Answers from your chat assistant"},
	},
}

// WhatsAppCategories returns the categories in display order.
func WhatsAppCategories() []WhatsAppCategory {
	return append([]WhatsAppCategory(nil), whatsAppCategories...)
}

// ServiceForMetaCategory maps a Meta category (any case) to its credit service.
func ServiceForMetaCategory(meta string) (string, bool) {
	for _, c := range whatsAppCategories {
		if strings.EqualFold(c.MetaCategory, meta) {
			return c.Service, true
		}
	}
	return "", false
}

func whatsAppCategoryByService(service string) (WhatsAppCategory, bool) {
	for _, c := range whatsAppCategories {
		if c.Service == service {
			return c, true
		}
	}
	return WhatsAppCategory{}, false
}

// templateIndex is built once from the embedded manifest.
type templateIndex struct {
	byName map[string]string // template name -> Meta category
	byStem map[string]string // name without its version suffix -> category, when unambiguous
	byID   map[string]string // worker template id (hadia/gift_owner_alert) -> category, when unambiguous
}

var (
	indexOnce sync.Once
	index     templateIndex

	versionSuffix = regexp.MustCompile(`(_v[0-9]+)?(_btn)?$`)
	sourceID      = regexp.MustCompile(`whatsapp/([a-z0-9_]+/[a-z0-9_]+)\.txt`)
)

func templateStem(name string) string { return versionSuffix.ReplaceAllString(name, "") }

// templateIDCategories cover worker template ids whose manifest source names a different file,
// and ids that never carry a template of their own.
var templateIDCategories = map[string]string{
	"auth/otp_verification": "AUTHENTICATION",
	"auth/password_reset":   "AUTHENTICATION",
	"broadcast/message":     "MARKETING",
	"whatsapp/inbox_reply":  "SERVICE",
}

func loadIndex() templateIndex {
	indexOnce.Do(func() {
		index = templateIndex{byName: map[string]string{}, byStem: map[string]string{}, byID: map[string]string{}}
		defs, err := templatesync.LoadManifest()
		if err != nil {
			return
		}
		ambiguous := func(m map[string]string, k, cat string) {
			if prev, ok := m[k]; ok && prev != cat {
				m[k] = "" // two categories: no answer from this key
				return
			}
			if _, ok := m[k]; !ok {
				m[k] = cat
			}
		}
		for _, d := range defs {
			cat := strings.ToUpper(d.Category)
			index.byName[d.Name] = cat
			ambiguous(index.byStem, templateStem(d.Name), cat)
			if m := sourceID.FindStringSubmatch(d.Source); m != nil {
				ambiguous(index.byID, m[1], cat)
			}
		}
		for id, cat := range templateIDCategories {
			index.byID[id] = cat
		}
	})
	return index
}

// metaCategoryForTemplateName finds a template's Meta category by its exact name, else by its
// name without the version suffix (an older version of the same template).
func metaCategoryForTemplateName(name string) (string, bool) {
	idx := loadIndex()
	if cat, ok := idx.byName[name]; ok && cat != "" {
		return cat, true
	}
	if cat, ok := idx.byStem[templateStem(name)]; ok && cat != "" {
		return cat, true
	}
	return "", false
}

func metaCategoryForTemplateID(id string) (string, bool) {
	cat, ok := loadIndex().byID[id]
	return cat, ok && cat != ""
}

// WhatsAppServiceFor returns the credit service a WhatsApp send spends. templateName is the Meta
// template the send uses (metadata template_name); templateID is the worker's notification id.
//   - No template: a free-form message, which Meta delivers only inside the 24 hour customer
//     service window, so it is a reply to a customer.
//   - A template: its manifest category, by name, then by an older version's name, then by the
//     notification id.
//
// resolved is false when nothing matched; the send is then billed as utility and the caller
// should log it (TestEveryTemplateTheCodeSendsResolves keeps that from happening for our own
// templates).
func WhatsAppServiceFor(templateName, templateID string) (service string, resolved bool) {
	if strings.TrimSpace(templateName) == "" {
		return ServiceWhatsAppService, true
	}
	if cat, ok := metaCategoryForTemplateName(templateName); ok {
		if svc, ok := ServiceForMetaCategory(cat); ok {
			return svc, true
		}
	}
	if cat, ok := metaCategoryForTemplateID(templateID); ok {
		if svc, ok := ServiceForMetaCategory(cat); ok {
			return svc, true
		}
	}
	return ServiceWhatsAppUtility, false
}

// WhatsAppServiceForMessage is WhatsAppServiceFor for a queued message.
func WhatsAppServiceForMessage(msg messaging.Message) (string, bool) {
	name, _ := msg.Metadata["template_name"].(string)
	return WhatsAppServiceFor(name, msg.TemplateID)
}

// ServiceForMessage is the credits service a message spends, "" for channels that are not paid.
func ServiceForMessage(msg messaging.Message) string {
	switch strings.ToLower(msg.Channel) {
	case "sms":
		return ServiceSMS
	case "whatsapp":
		svc, _ := WhatsAppServiceForMessage(msg)
		return svc
	}
	return ""
}
