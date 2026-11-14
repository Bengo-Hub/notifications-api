package handlers

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bengobox/notifications-api/internal/providers"
)

// A number saved without its WhatsApp account answers 409 with what to do, on both the templates
// and the profile, instead of a 400 asking for an Embedded Signup reconnect.
func TestUnlinkedNumberAnswersConflict(t *testing.T) {
	th, _ := newTenantTemplatesHandler(t, "http://unused", providers.ErrTenantNumberUnlinked)
	w := serveAs(th.TenantStatus, http.MethodGet, "/api/v1/settings/whatsapp/templates", "", "t1", false, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "portfolio") {
		t.Fatalf("templates: status %d body %s, want 409 naming the portfolio link", w.Code, w.Body.String())
	}

	ph, _ := newProfileHandler(t, "http://unused", fmt.Errorf("load: %w", providers.ErrTenantNumberUnlinked))
	w = serveAs(ph.Get, http.MethodGet, "/api/v1/settings/whatsapp/profile", "", "t1", false, "")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "portfolio") || strings.Contains(w.Body.String(), "reconnect") {
		t.Fatalf("profile: status %d body %s, want 409 naming the portfolio link", w.Code, w.Body.String())
	}
}

// The manual Phone Number ID field must not leave a tenant with a number and nothing to send it
// with: a platform owner is sent to the portfolio link, anyone else is told how to connect.
func TestManualWhatsAppNumberRefusal(t *testing.T) {
	const platformID = "platform-id"
	cases := []struct {
		name     string
		tenant   string
		po       bool
		existing map[string]string
		settings map[string]string
		refused  bool
		mention  string
	}{
		{name: "platform owner types a number for a tenant", tenant: "hadia", po: true, existing: map[string]string{}, settings: map[string]string{"phone_number_id": "1471029032759769"}, refused: true, mention: "Link a number from our portfolio"},
		{name: "tenant types a number", tenant: "hadia", existing: map[string]string{}, settings: map[string]string{"phone_number_id": "1"}, refused: true, mention: "Embedded Signup"},
		{name: "resave of a half-saved number", tenant: "hadia", existing: map[string]string{"phone_number_id": "1"}, settings: map[string]string{"api_version": "v26.0"}, refused: true},
		{name: "number with its own token", tenant: "t1", existing: map[string]string{}, settings: map[string]string{"phone_number_id": "1", "waba_id": "2", "access_token": "tok"}},
		{name: "masked token kept from before", tenant: "t1", existing: map[string]string{"phone_number_id": "1", "access_token": "enc"}, settings: map[string]string{"phone_number_id": "3", "access_token": "••••••••"}},
		{name: "managed link resaved", tenant: "t1", existing: map[string]string{"phone_number_id": "1", "waba_id": "2", "token_source": "platform"}, settings: map[string]string{"api_version": "v26.0"}},
		{name: "no number at all", tenant: "t1", existing: map[string]string{}, settings: map[string]string{"api_version": "v26.0"}},
		{name: "the platform's own row", tenant: platformID, po: true, existing: map[string]string{}, settings: map[string]string{"phone_number_id": "1"}},
	}
	for _, c := range cases {
		msg := manualWhatsAppNumberRefusal("whatsapp", "meta_cloud", c.tenant, platformID, c.po, c.existing, c.settings)
		if (msg != "") != c.refused {
			t.Errorf("%s: refusal %q, want refused=%v", c.name, msg, c.refused)
		}
		if c.mention != "" && !strings.Contains(msg, c.mention) {
			t.Errorf("%s: refusal %q should mention %q", c.name, msg, c.mention)
		}
	}
	if manualWhatsAppNumberRefusal("sms", "africastalking", "t1", platformID, false, map[string]string{}, map[string]string{"phone_number_id": "1"}) != "" {
		t.Error("other providers are not affected")
	}
}
