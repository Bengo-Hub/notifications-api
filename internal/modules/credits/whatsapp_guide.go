package credits

import (
	"sort"
	"strings"
)

// The tenant guide answers "which of my messages use which WhatsApp credits", in plain words, from
// the messages the tenant actually sent (delivery log template ids), so it knows what to buy.

// GuideMessage is one kind of message the tenant sent.
type GuideMessage struct {
	TemplateID string `json:"template_id"`
	Label      string `json:"label"`
	Sent       int    `json:"sent"`
}

// CategoryGuide is one WhatsApp credit category with the tenant's own messages in it.
type CategoryGuide struct {
	Service      string         `json:"service"`
	MetaCategory string         `json:"meta_category"`
	Name         string         `json:"name"`
	Explainer    string         `json:"explainer"`
	Examples     []string       `json:"examples"`
	YourMessages []GuideMessage `json:"your_messages"`
}

// templateIDService places a delivery log template id in a category. The log keeps the
// notification id, not the Meta template, so this goes by the manifest's source file and a few
// known ids; anything else is an order, payment or account update.
func templateIDService(id string) string {
	if cat, ok := metaCategoryForTemplateID(id); ok {
		if svc, ok := ServiceForMetaCategory(cat); ok {
			return svc
		}
	}
	return ServiceWhatsAppUtility
}

// humanizeTemplateID turns "hadia/gift_owner_alert" into "Gift owner alert".
func humanizeTemplateID(id string) string {
	name := id
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
	if name == "" {
		return id
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// BuildWhatsAppGuide groups the tenant's sent message kinds (template id to count) by category,
// most sent first. label names a template id for people ("" falls back to a readable id).
func BuildWhatsAppGuide(sent map[string]int, label func(templateID string) string) []CategoryGuide {
	byService := map[string][]GuideMessage{}
	for id, n := range sent {
		l := ""
		if label != nil {
			l = label(id)
		}
		if l == "" {
			l = humanizeTemplateID(id)
		}
		svc := templateIDService(id)
		byService[svc] = append(byService[svc], GuideMessage{TemplateID: id, Label: l, Sent: n})
	}
	out := make([]CategoryGuide, 0, len(whatsAppCategories))
	for _, c := range whatsAppCategories {
		msgs := byService[c.Service]
		sort.Slice(msgs, func(i, j int) bool {
			if msgs[i].Sent != msgs[j].Sent {
				return msgs[i].Sent > msgs[j].Sent
			}
			return msgs[i].TemplateID < msgs[j].TemplateID
		})
		if msgs == nil {
			msgs = []GuideMessage{}
		}
		out = append(out, CategoryGuide{
			Service: c.Service, MetaCategory: c.MetaCategory, Name: c.Name, Explainer: c.Explainer,
			Examples: append([]string(nil), c.Examples...), YourMessages: msgs,
		})
	}
	return out
}
