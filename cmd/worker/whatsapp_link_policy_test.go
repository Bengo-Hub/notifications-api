package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestApplyWhatsAppLinkButton(t *testing.T) {
	meta := map[string]interface{}{}
	applyWhatsAppLinkButton(meta, map[string]interface{}{"action_link": "https://pricing.codevertexafrica.com/settings/subscription"})
	if meta["cta_button_url"] != "https://pricing.codevertexafrica.com/settings/subscription" || meta["cta_button_text"] != "View" {
		t.Fatalf("free-form link must become a button: %#v", meta)
	}

	meta = map[string]interface{}{"template_name": "ordering_order_ready_v3_btn"}
	applyWhatsAppLinkButton(meta, map[string]interface{}{"order_link": "https://ordering.codevertexafrica.com/x"})
	if _, ok := meta["cta_button_url"]; ok {
		t.Fatalf("template sends carry their own button")
	}

	meta = map[string]interface{}{}
	applyWhatsAppLinkButton(meta, map[string]interface{}{"order_link": "-"})
	if _, ok := meta["cta_button_url"]; ok {
		t.Fatalf("a non-URL value must not become a button")
	}
}

// Link policy lint: no WhatsApp text body may show a link; links go out as buttons.
func TestWhatsAppBodiesCarryNoRawLinks(t *testing.T) {
	linkVar := regexp.MustCompile(`\{\{[^}]*(_link|_url|link)\b[^}]*\}\}|https?://`)
	files, err := filepath.Glob("../../templates/whatsapp/*/*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no WhatsApp templates found: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := linkVar.Find(b); loc != nil {
			t.Errorf("%s shows a raw link (%s); send it as a button instead", f, loc)
		}
	}
}
