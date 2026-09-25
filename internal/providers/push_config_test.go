package providers

import "testing"

func TestPushSettingsFrom(t *testing.T) {
	ps := pushSettingsFrom("platform", map[string]string{
		"service_account":         `{"type":"service_account","project_id":"codevertex-push"}`,
		"web_api_key":             "public-key",
		"web_messaging_sender_id": "123",
		"web_app_id":              "1:123:web:abc",
		"web_vapid_key":           "BPublicVapid",
	})
	if !ps.Ready() || ps.ProjectID != "codevertex-push" {
		t.Fatalf("project id should come from the service account: %#v", ps)
	}
	if !ps.Web.Complete() || ps.Web.ProjectID != "codevertex-push" || ps.Web.AuthDomain != "codevertex-push.firebaseapp.com" {
		t.Fatalf("web config should share the project and derive the auth domain: %#v", ps.Web)
	}
}

func TestPushSettingsFrom_Incomplete(t *testing.T) {
	// A tenant with only web values (no service account) is not a usable tier on its own.
	ps := pushSettingsFrom("tenant", map[string]string{"web_api_key": "k", "project_id": "p"})
	if ps.Ready() {
		t.Fatalf("no service account must not be ready")
	}
	if pushSettingsFrom("env", map[string]string{}).Web.Complete() {
		t.Fatalf("empty web config must not be complete")
	}
}
