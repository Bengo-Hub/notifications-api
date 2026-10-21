package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// metaSignatureHeader carries Meta's HMAC-SHA256 of the raw webhook body, keyed with the app
// secret, as "sha256=<hex>".
const metaSignatureHeader = "X-Hub-Signature-256"

// maxWhatsAppWebhookBody caps how much of a webhook body is read. Meta's notifications are a few
// KB; the cap stops an unauthenticated caller from making the handler buffer arbitrary payloads.
const maxWhatsAppWebhookBody = 1 << 20

// unsignedWarnEvery limits the "no app secret configured" warning to one line per interval.
const unsignedWarnEvery = 10 * time.Minute

// metaSignature decides whether an inbound WhatsApp webhook is genuine.
type metaSignature struct {
	secret   func(ctx context.Context) string
	require  bool
	lastWarn atomic.Int64
}

// WithMetaAppSecret turns on X-Hub-Signature-256 checks for the WhatsApp webhook. secret returns
// the Meta app secret (the platform whatsapp/meta_cloud "app_secret" setting); WHATSAPP_APP_SECRET
// is used when it returns "". With a secret, a missing or wrong signature is rejected. Without
// one, requests are accepted with a periodic warning unless require is true, so turning this on
// never cuts off live traffic before the secret is saved.
func (h *WebhookHandler) WithMetaAppSecret(secret func(ctx context.Context) string, require bool) *WebhookHandler {
	h.signature = &metaSignature{secret: secret, require: require}
	return h
}

// WhatsAppSignatureRequired reports whether WHATSAPP_WEBHOOK_REQUIRE_SIGNATURE asks for strict
// mode (reject when no app secret is configured).
func WhatsAppSignatureRequired() bool {
	return strings.EqualFold(os.Getenv("WHATSAPP_WEBHOOK_REQUIRE_SIGNATURE"), "true")
}

// verified reports whether the body may be processed. A nil receiver (checks not wired) accepts.
func (s *metaSignature) verified(ctx context.Context, header string, body []byte, log *zap.Logger) bool {
	if s == nil {
		return true
	}
	secret := ""
	if s.secret != nil {
		secret = s.secret(ctx)
	}
	if secret == "" {
		secret = os.Getenv("WHATSAPP_APP_SECRET")
	}
	if secret == "" {
		if s.require {
			log.Warn("whatsapp webhook rejected: signature required but no Meta app secret is configured")
			return false
		}
		s.warnUnsigned(log)
		return true
	}
	if !validMetaSignature(secret, header, body) {
		log.Warn("whatsapp webhook rejected: invalid or missing " + metaSignatureHeader)
		return false
	}
	return true
}

func (s *metaSignature) warnUnsigned(log *zap.Logger) {
	now := time.Now().UnixNano()
	last := s.lastWarn.Load()
	if last != 0 && time.Duration(now-last) < unsignedWarnEvery {
		return
	}
	if s.lastWarn.CompareAndSwap(last, now) {
		log.Warn("whatsapp webhook accepted without signature check: save the Meta app secret as whatsapp/meta_cloud app_secret on the platform providers page")
	}
}

// validMetaSignature checks header ("sha256=<hex>") against HMAC-SHA256(body, secret) in
// constant time.
func validMetaSignature(secret, header string, body []byte) bool {
	got, ok := strings.CutPrefix(strings.TrimSpace(header), "sha256=")
	if !ok || got == "" {
		return false
	}
	sig, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}
