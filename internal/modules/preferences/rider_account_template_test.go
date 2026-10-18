package preferences

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// logistics/rider_account carries a new internal rider's temporary password, and for a rider
// who already had a platform account it must not show a password row at all.
func TestRiderAccountTemplateRenders(t *testing.T) {
	tpl, err := template.ParseFiles("../../../templates/email/logistics/rider_account.html")
	if err != nil {
		t.Fatal(err)
	}
	render := func(data map[string]any) string {
		var out bytes.Buffer
		if err := tpl.ExecuteTemplate(&out, "content", data); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	base := map[string]any{"name": "Phelix", "email": "rider@example.com", "login_url": "https://riderapp.example.com/urban-loft"}

	withPassword := map[string]any{"temp_password": "Tmp-9x2Q"}
	for k, v := range base {
		withPassword[k] = v
	}
	html := render(withPassword)
	for _, want := range []string{"Tmp-9x2Q", "rider@example.com", "choose your own password", "https://riderapp.example.com/urban-loft"} {
		if !strings.Contains(html, want) {
			t.Errorf("new account email lacks %q", want)
		}
	}

	html = render(base)
	if strings.Contains(html, "Temporary password") {
		t.Error("an existing account must not get a password row")
	}
	if !strings.Contains(html, "password you already use") {
		t.Error("an existing account must be told to use their own password")
	}
}
