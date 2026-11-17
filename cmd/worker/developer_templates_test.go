package main

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// The developer-access emails are sent by auth-api through /notifications/messages, so no
// consumer test covers them. These render each template with the data auth-api sends.
func TestDeveloperAccessTemplatesRender(t *testing.T) {
	cases := []struct {
		id       string
		data     map[string]any
		want     []string
		mustSkip []string
	}{
		{
			id: "platform/developer_provisioned",
			data: map[string]any{
				"requester_name": "Amina", "tenant_slug": "dev-amina", "client_id": "app_0a1b2c3d4e5f",
				"console_url": "https://accounts.example/dashboard/developer/apps", "docs_url": "https://accounts.example/docs",
			},
			want:     []string{"Amina", "app_0a1b2c3d4e5f", "Generate consumer secret", "grant_type=client_credentials", "/dashboard/developer/apps"},
			mustSkip: []string{"bng_app_", "sandbox_key"},
		},
		{
			id: "platform/etims_integration_request",
			data: map[string]any{
				"requester_name": "Amina", "requester_email": "amina@example.com", "request_type": "etims_integration",
				"request_type_label": "KRA eTIMS invoices", "integration_mode": "assisted", "notes": "Two branches",
			},
			want: []string{"Amina"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			tpl, err := template.ParseFiles("../../templates/email/" + tc.id + ".html")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			var out bytes.Buffer
			if err := tpl.ExecuteTemplate(&out, "content", tc.data); err != nil {
				t.Fatalf("render: %v", err)
			}
			html := out.String()
			for _, w := range tc.want {
				if !strings.Contains(html, w) {
					t.Errorf("rendered %s lacks %q", tc.id, w)
				}
			}
			for _, s := range tc.mustSkip {
				if strings.Contains(html, s) {
					t.Errorf("rendered %s must not contain %q", tc.id, s)
				}
			}
		})
	}
}
