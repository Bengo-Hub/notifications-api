package main

import "testing"

func TestShouldEmailOnStatusUpdate(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{"rejected", true},
		{"pending", false},
		{"kyb_pending", false},
		{"kyb_approved", false},
		{"agreement_pending", false},
		{"approved", false}, // approved has its own dedicated event/handler, never status_updated
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := shouldEmailOnStatusUpdate(tt.status); got != tt.want {
				t.Errorf("shouldEmailOnStatusUpdate(%q) = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestBuildResellerDecisionMessage_MissingContactEmail_Skips(t *testing.T) {
	_, ok := buildResellerDecisionMessage("auth/reseller_rejected", "subject", map[string]any{
		"business_name": "Acme Resellers Ltd",
	})
	if ok {
		t.Error("expected ok=false when payload has no contact_email")
	}
}

func TestBuildResellerDecisionMessage_HappyPath(t *testing.T) {
	msg, ok := buildResellerDecisionMessage("auth/reseller_approved", "You're a Certified Codevertex Reseller Partner", map[string]any{
		"application_id": "11111111-1111-1111-1111-111111111111",
		"contact_email":  "partner@acme.example",
		"business_name":  "Acme Resellers Ltd",
		"requested_tier": "registered",
		"tenant_id":      "22222222-2222-2222-2222-222222222222",
	})
	if !ok {
		t.Fatal("expected ok=true for a payload with a valid contact_email")
	}
	if len(msg.To) != 1 || msg.To[0] != "partner@acme.example" {
		t.Errorf("To = %v, want [partner@acme.example]", msg.To)
	}
	if msg.TemplateID != "auth/reseller_approved" {
		t.Errorf("TemplateID = %q, want auth/reseller_approved", msg.TemplateID)
	}
	if msg.TenantID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("TenantID = %q, want the payload's tenant_id", msg.TenantID)
	}
	if msg.Data["business_name"] != "Acme Resellers Ltd" || msg.Data["name"] != "Acme Resellers Ltd" {
		t.Errorf("Data = %v, want name/business_name = Acme Resellers Ltd", msg.Data)
	}
	if msg.Data["requested_tier"] != "registered" {
		t.Errorf("Data[requested_tier] = %v, want registered", msg.Data["requested_tier"])
	}
	if msg.Metadata["subject"] != "You're a Certified Codevertex Reseller Partner" {
		t.Errorf("Metadata[subject] = %v, want the default subject", msg.Metadata["subject"])
	}
	wantIdempotencyKey := "reseller-application-auth/reseller_approved-11111111-1111-1111-1111-111111111111"
	if msg.IdempotencyKey != wantIdempotencyKey {
		t.Errorf("IdempotencyKey = %q, want %q", msg.IdempotencyKey, wantIdempotencyKey)
	}
}

func TestBuildResellerDecisionMessage_FallsBackToEmailWhenNameMissing(t *testing.T) {
	msg, ok := buildResellerDecisionMessage("auth/reseller_rejected", "subject", map[string]any{
		"contact_email": "no-name@example.com",
	})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if msg.Data["name"] != "no-name@example.com" {
		t.Errorf("Data[name] = %v, want the email fallback", msg.Data["name"])
	}
}
