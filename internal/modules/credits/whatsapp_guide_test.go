package credits

import (
	"strings"
	"testing"
)

func TestWhatsAppGuidePutsTheTenantsOwnMessagesInTheirCategory(t *testing.T) {
	sent := map[string]int{
		"hadia/gift_owner_alert": 12,
		"hadia/gift_receipt":     30,
		"auth/otp_verification":  4,
		"whatsapp/inbox_reply":   7,
		"broadcast/message":      2,
		"custom/unknown":         1,
	}
	labels := map[string]string{"hadia/gift_owner_alert": "You received a gift"}
	guide := BuildWhatsAppGuide(sent, func(id string) string { return labels[id] })

	if len(guide) != len(WhatsAppCategories()) {
		t.Fatalf("one entry per category, got %d", len(guide))
	}
	by := map[string]CategoryGuide{}
	for _, g := range guide {
		by[g.Service] = g
		if g.Name == "" || g.Explainer == "" {
			t.Errorf("%s: plain name and explanation are required: %+v", g.Service, g)
		}
		if len(g.Examples) == 0 {
			t.Errorf("%s: a tenant with no sends still needs examples", g.Service)
		}
	}
	util := by[ServiceWhatsAppUtility]
	if len(util.YourMessages) < 2 || util.YourMessages[0].TemplateID != "hadia/gift_receipt" || util.YourMessages[0].Sent != 30 {
		t.Fatalf("most sent first: %+v", util.YourMessages)
	}
	var gift *GuideMessage
	for i := range util.YourMessages {
		if util.YourMessages[i].TemplateID == "hadia/gift_owner_alert" {
			gift = &util.YourMessages[i]
		}
	}
	if gift == nil || gift.Label != "You received a gift" {
		t.Fatalf("registry label is used: %+v", util.YourMessages)
	}
	for _, m := range util.YourMessages {
		if m.Label == "" || strings.Contains(m.Label, "/") {
			t.Errorf("label %q must be readable", m.Label)
		}
	}
	if a := by[ServiceWhatsAppAuthentication].YourMessages; len(a) != 1 || a[0].TemplateID != "auth/otp_verification" {
		t.Errorf("login codes: %+v", a)
	}
	if s := by[ServiceWhatsAppService].YourMessages; len(s) != 1 || s[0].TemplateID != "whatsapp/inbox_reply" {
		t.Errorf("replies: %+v", s)
	}
	if m := by[ServiceWhatsAppMarketing].YourMessages; len(m) != 1 || m[0].TemplateID != "broadcast/message" {
		t.Errorf("promotions: %+v", m)
	}
}
