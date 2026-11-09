package main

import (
	"html/template"
	"path/filepath"
	"strings"
	"testing"
)

// A new account's welcome carries a one-time set-password link instead of telling the person to use
// a temporary password nobody sent them (10 November 2026); without a link it keeps the old wording.
func TestWelcomeSetPasswordLink(t *testing.T) {
	tpl, err := template.ParseFiles(filepath.Join("..", "..", "templates", "email", "auth", "welcome.html"))
	if err != nil {
		t.Fatal(err)
	}
	render := func(data map[string]any) string {
		var b strings.Builder
		if err := tpl.ExecuteTemplate(&b, "content", data); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	link := "https://staging.hadiafrica.com/hadia/auth/reset-password?email=a%40b.c&token=abc"
	html := render(map[string]any{"name": "Wanjiru", "brand_name": "Hadia", "set_password_url": link,
		"login_url": "https://accounts.codevertexafrica.com", "must_change_password": true})
	if !strings.Contains(html, "Set your password") || !strings.Contains(html, "72 hours") || strings.Contains(html, "temporary password your administrator") {
		t.Fatalf("set-password welcome: %s", html)
	}
	old := render(map[string]any{"name": "Wanjiru", "login_url": "https://accounts.codevertexafrica.com", "must_change_password": true})
	if !strings.Contains(old, "temporary password your administrator") || strings.Contains(old, "Set your password") {
		t.Fatalf("fallback welcome: %s", old)
	}
}
