package credits

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/messaging"
	"github.com/bengobox/notifications-api/internal/whatsapp/templatesync"
)

func TestWhatsAppServiceForTemplate(t *testing.T) {
	cases := []struct {
		name, templateName, templateID string
		want                           string
		resolved                       bool
	}{
		{"order update template", "ordering_order_placed_v3_btn", "ordering/order_placed", ServiceWhatsAppUtility, true},
		{"hadia gift alert", "hadia_gift_owner_alert_v1_btn", "hadia/gift_owner_alert", ServiceWhatsAppUtility, true},
		{"login code", "auth_otp", "auth/otp", ServiceWhatsAppAuthentication, true},
		{"occasion greeting", "occasion_christmas_v2", "broadcast/message", ServiceWhatsAppMarketing, true},
		{"old occasion version resolves by its stem", "occasion_christmas_v1", "broadcast/message", ServiceWhatsAppMarketing, true},
		{"broadcast offer", "broadcast_update_v1", "broadcast/message", ServiceWhatsAppMarketing, true},
		// No template means a free-form message, which Meta only delivers inside the 24 hour window.
		{"free-form reply", "", "whatsapp/inbox_reply", ServiceWhatsAppService, true},
		{"free-form notification", "", "hadia/gift_owner_alert", ServiceWhatsAppService, true},
		// A template unknown to the manifest falls back to its template id, then to utility.
		{"unknown template, known id", "tenant_custom_v1", "hadia/gift_owner_alert", ServiceWhatsAppUtility, true},
		{"unknown template and id", "tenant_custom_v1", "custom/thing", ServiceWhatsAppUtility, false},
	}
	for _, c := range cases {
		got, ok := WhatsAppServiceFor(c.templateName, c.templateID)
		if got != c.want || ok != c.resolved {
			t.Errorf("%s: got %q %v, want %q %v", c.name, got, ok, c.want, c.resolved)
		}
	}
}

func TestWhatsAppServiceForMessageReadsTheTemplateName(t *testing.T) {
	msg := messaging.Message{Channel: "whatsapp", TemplateID: "broadcast/message",
		Metadata: map[string]interface{}{"template_name": "occasion_new_year_v2"}}
	if got, _ := WhatsAppServiceForMessage(msg); got != ServiceWhatsAppMarketing {
		t.Fatalf("got %q", got)
	}
	msg.Metadata = nil
	if got, _ := WhatsAppServiceForMessage(msg); got != ServiceWhatsAppService {
		t.Fatalf("no template is a free-form reply, got %q", got)
	}
	if got := ServiceForMessage(messaging.Message{Channel: "sms"}); got != ServiceSMS {
		t.Fatalf("sms: %q", got)
	}
	if got := ServiceForMessage(messaging.Message{Channel: "email"}); got != "" {
		t.Fatalf("email is not paid: %q", got)
	}
}

func TestEveryManifestCategoryHasACreditService(t *testing.T) {
	defs, err := templatesync.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		if _, ok := ServiceForMetaCategory(d.Category); !ok {
			t.Errorf("%s: category %q has no credit service", d.Name, d.Category)
		}
	}
}

func TestRefusalMessageUsesPlainCategoryNames(t *testing.T) {
	for _, c := range WhatsAppCategories() {
		msg := RefusalMessage(c.Service, ReasonInsufficient)
		if !strings.Contains(msg, c.Name) {
			t.Errorf("%s: %q must name the category in plain words", c.Service, msg)
		}
		for _, jargon := range []string{"MARKETING", "UTILITY", "AUTHENTICATION", "utility"} {
			if strings.Contains(msg, jargon) {
				t.Errorf("%s: %q shows Meta jargon", c.Service, msg)
			}
		}
	}
	// SMS is credits only: running out says plainly what to buy.
	if msg := RefusalMessage(ServiceSMS, ReasonInsufficient); !strings.Contains(msg, "buy SMS credits") || strings.Contains(msg, "overage") {
		t.Errorf("sms: %q", msg)
	}
}

var (
	// Template names assigned in code: struct fields, metadata keys and the billing spec field.
	templateAssign = regexp.MustCompile(`(?:WATemplate|WhatsAppButtonTemplate|WhatsAppReviewTemplate|WhatsAppTemplate|opsWhatsAppTemplate|"template_name"|"template_fallback_name"|template)\s*[:=]+\s*"([a-z0-9_]+)"`)
	// Any literal shaped like a versioned template name ("x_v2", "x_v1_btn").
	templateShape = regexp.MustCompile(`"([a-z][a-z0-9_]*_v[0-9]+(?:_btn)?)"`)
)

// TestEveryTemplateTheCodeSendsResolves is the manifest lint for billing: every WhatsApp template
// name the code sends must map to a category through the manifest, so no send is silently billed
// as utility by the fallback.
func TestEveryTemplateTheCodeSendsResolves(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	names := map[string]string{}
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "ent" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, re := range []*regexp.Regexp{templateAssign, templateShape} {
				for _, m := range re.FindAllStringSubmatch(string(src), -1) {
					names[m[1]] = path
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(names) < 30 {
		t.Fatalf("found only %d template names; the scan is broken", len(names))
	}
	for name, path := range names {
		if _, ok := metaCategoryForTemplateName(name); !ok {
			t.Errorf("%s (in %s) is not in templates.json: its sends would be billed as a guess", name, path)
		}
	}
}
