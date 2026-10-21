package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func metaSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestValidMetaSignature(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[]}`)
	good := metaSign("s3cret", body)

	cases := []struct {
		name   string
		secret string
		header string
		body   []byte
		want   bool
	}{
		{"valid signature", "s3cret", good, body, true},
		{"valid with surrounding spaces", "s3cret", "  " + good + " ", body, true},
		{"wrong secret", "other", good, body, false},
		{"body changed after signing", "s3cret", good, append(append([]byte{}, body...), ' '), false},
		{"missing header", "s3cret", "", body, false},
		{"missing sha256 prefix", "s3cret", good[len("sha256="):], body, false},
		{"sha1 style header", "s3cret", "sha1=abcdef", body, false},
		{"empty hex after prefix", "s3cret", "sha256=", body, false},
		{"non hex signature", "s3cret", "sha256=zz", body, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validMetaSignature(tc.secret, tc.header, tc.body); got != tc.want {
				t.Fatalf("validMetaSignature() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMetaSignatureVerified(t *testing.T) {
	log := zap.NewNop()
	body := []byte(`{"entry":[]}`)
	ctx := context.Background()
	fixed := func(v string) func(context.Context) string { return func(context.Context) string { return v } }

	t.Run("nil checker accepts (not wired)", func(t *testing.T) {
		var s *metaSignature
		if !s.verified(ctx, "", body, log) {
			t.Fatal("nil checker must accept")
		}
	})
	t.Run("configured secret and good signature", func(t *testing.T) {
		s := &metaSignature{secret: fixed("k")}
		if !s.verified(ctx, metaSign("k", body), body, log) {
			t.Fatal("good signature rejected")
		}
	})
	t.Run("configured secret and bad signature", func(t *testing.T) {
		s := &metaSignature{secret: fixed("k")}
		if s.verified(ctx, metaSign("wrong", body), body, log) {
			t.Fatal("bad signature accepted")
		}
	})
	t.Run("configured secret and no header", func(t *testing.T) {
		s := &metaSignature{secret: fixed("k")}
		if s.verified(ctx, "", body, log) {
			t.Fatal("unsigned body accepted while a secret is configured")
		}
	})
	t.Run("no secret, lenient mode accepts", func(t *testing.T) {
		t.Setenv("WHATSAPP_APP_SECRET", "")
		s := &metaSignature{secret: fixed("")}
		if !s.verified(ctx, "", body, log) {
			t.Fatal("lenient mode must accept when no secret is configured")
		}
		// a second call inside the warn window must also accept (only the warning is rate limited)
		if !s.verified(ctx, "", body, log) {
			t.Fatal("lenient mode must keep accepting")
		}
	})
	t.Run("no secret, strict mode rejects", func(t *testing.T) {
		t.Setenv("WHATSAPP_APP_SECRET", "")
		s := &metaSignature{secret: fixed(""), require: true}
		if s.verified(ctx, metaSign("anything", body), body, log) {
			t.Fatal("strict mode must reject when no secret is configured")
		}
	})
	t.Run("env fallback when the setting is empty", func(t *testing.T) {
		t.Setenv("WHATSAPP_APP_SECRET", "envkey")
		s := &metaSignature{secret: fixed("")}
		if !s.verified(ctx, metaSign("envkey", body), body, log) {
			t.Fatal("env secret not used")
		}
		if s.verified(ctx, metaSign("other", body), body, log) {
			t.Fatal("env secret set but a wrong signature was accepted")
		}
	})
	t.Run("stored setting wins over env", func(t *testing.T) {
		t.Setenv("WHATSAPP_APP_SECRET", "envkey")
		s := &metaSignature{secret: fixed("dbkey")}
		if s.verified(ctx, metaSign("envkey", body), body, log) {
			t.Fatal("env secret must not override the stored setting")
		}
		if !s.verified(ctx, metaSign("dbkey", body), body, log) {
			t.Fatal("stored secret not used")
		}
	})
}

func TestWhatsAppSignatureRequired(t *testing.T) {
	for _, tc := range []struct {
		val  string
		want bool
	}{{"true", true}, {"TRUE", true}, {"false", false}, {"", false}, {"1", false}} {
		t.Setenv("WHATSAPP_WEBHOOK_REQUIRE_SIGNATURE", tc.val)
		if got := WhatsAppSignatureRequired(); got != tc.want {
			t.Fatalf("WhatsAppSignatureRequired(%q) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// The handler must reject a forged body before touching any payload logic, and still ack a
// genuine one with 200 (Meta disables subscriptions that keep failing).
func TestWhatsAppIncomingSignatureGate(t *testing.T) {
	body := []byte(`{"object":"whatsapp_business_account","entry":[]}`)
	h := NewWebhookHandler(nil, zap.NewNop(), "https://example.test", nil).
		WithMetaAppSecret(func(context.Context) string { return "k" }, false)

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{"genuine", metaSign("k", body), http.StatusOK},
		{"forged", metaSign("attacker", body), http.StatusUnauthorized},
		{"unsigned", "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/whatsapp/meta", bytes.NewReader(body))
			if tc.header != "" {
				req.Header.Set(metaSignatureHeader, tc.header)
			}
			rec := httptest.NewRecorder()
			h.WhatsAppIncoming(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// A body above the cap is truncated before hashing, so its signature no longer matches.
func TestWhatsAppIncomingOversizedBodyRejected(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxWhatsAppWebhookBody+10)
	h := NewWebhookHandler(nil, zap.NewNop(), "https://example.test", nil).
		WithMetaAppSecret(func(context.Context) string { return "k" }, false)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/whatsapp/meta", bytes.NewReader(body))
	req.Header.Set(metaSignatureHeader, metaSign("k", body))
	rec := httptest.NewRecorder()
	h.WhatsAppIncoming(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an oversized body", rec.Code)
	}
}
