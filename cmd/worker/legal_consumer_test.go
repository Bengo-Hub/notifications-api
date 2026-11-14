package main

import (
	"testing"
	"time"
)

func TestBuildLegalChangeMessage(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	msg, ok := buildLegalChangeMessage(map[string]any{
		"email": "a@b.co", "user_id": "u1", "doc_type": "TERMS_OF_SERVICE", "version": "3",
		"title": "Terms of Service", "summary": "Clearer refunds", "effective_date": "2026-11-29", "in_force": false,
		"document_url": "https://accounts.example/terms-of-service/3",
	}, now)
	if !ok {
		t.Fatal("valid payload refused")
	}
	if msg.TemplateID != "auth/legal_updated" || msg.To[0] != "a@b.co" || msg.Metadata["subject"] != "We are updating our Terms of Service" {
		t.Fatalf("message: %+v", msg)
	}
	if msg.IdempotencyKey != "auth-legal-TERMS_OF_SERVICE-3-u1" {
		t.Fatalf("idempotency key %q", msg.IdempotencyKey)
	}
	if _, ok := buildLegalChangeMessage(map[string]any{"email": "a@b.co", "version": "3"}, now); ok {
		t.Fatal("payload without a user accepted")
	}
	in, _ := buildLegalChangeMessage(map[string]any{"email": "a@b.co", "user_id": "u1", "version": "3", "in_force": true}, now)
	if in.Metadata["subject"] != "We have updated our Terms" {
		t.Fatalf("in-force subject %v", in.Metadata["subject"])
	}
}
