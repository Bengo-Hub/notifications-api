package push

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Standard Web Push (RFC 8030 + VAPID). The platform's default push channel when no Firebase
// project is configured: notifications-service holds its own VAPID key pair and the browser's
// own push service (Google, Mozilla, Apple) delivers, so no third-party account is needed.
// A device "token" for this provider is the browser's PushSubscription JSON
// ({"endpoint": ..., "keys": {"p256dh": ..., "auth": ...}}).

// WebPushConfig is the VAPID identity sends are signed with.
type WebPushConfig struct {
	PublicKey  string
	PrivateKey string
	// Subject is the contact push services may use about this sender (mailto: or https:).
	Subject string
}

// WebPushProvider sends to browser push subscriptions.
type WebPushProvider struct {
	cfg WebPushConfig
}

// NewWebPush creates a Web Push provider.
func NewWebPush(cfg WebPushConfig) *WebPushProvider { return &WebPushProvider{cfg: cfg} }

func (p *WebPushProvider) Name() string { return "webpush" }

// IsWebPushSubscription reports whether a stored device token is a browser PushSubscription
// (JSON) rather than an FCM registration token.
func IsWebPushSubscription(token string) bool {
	return strings.HasPrefix(strings.TrimSpace(token), "{")
}

// webPushPayload has the same shape FCM delivers, so the apps' service workers read either.
type webPushPayload struct {
	Notification struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"notification"`
	Data map[string]string `json:"data,omitempty"`
}

// SendPush sends to every PushSubscription in tokens; FCM tokens are skipped (another provider's).
// Subscriptions the push service reports gone (404/410) come back as UnregisteredTokensError.
func (p *WebPushProvider) SendPush(ctx context.Context, tokens []string, title, body string, data map[string]string) error {
	if p.cfg.PublicKey == "" || p.cfg.PrivateKey == "" {
		return fmt.Errorf("webpush: VAPID keys not configured")
	}
	var payload webPushPayload
	payload.Notification.Title = title
	payload.Notification.Body = body
	payload.Data = data
	msg, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	var lastErr error
	var dead []string
	delivered := 0
	for _, token := range tokens {
		if !IsWebPushSubscription(token) {
			continue
		}
		var sub webpush.Subscription
		if err := json.Unmarshal([]byte(token), &sub); err != nil || sub.Endpoint == "" {
			dead = append(dead, token)
			continue
		}
		resp, err := webpush.SendNotificationWithContext(ctx, msg, &sub, &webpush.Options{
			Subscriber:      p.cfg.Subject,
			VAPIDPublicKey:  p.cfg.PublicKey,
			VAPIDPrivateKey: p.cfg.PrivateKey,
			TTL:             3600,
			Urgency:         webpush.UrgencyHigh,
		})
		if err != nil {
			lastErr = fmt.Errorf("webpush: send: %w", err)
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			dead = append(dead, token)
		case resp.StatusCode >= 300:
			lastErr = fmt.Errorf("webpush: send failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
		default:
			delivered++
		}
	}
	if len(dead) > 0 {
		return &UnregisteredTokensError{Tokens: dead, Delivered: delivered, Other: lastErr}
	}
	return lastErr
}

// AccountInfo reports the VAPID identity in use (for the provider settings Test button).
func (p *WebPushProvider) AccountInfo(_ context.Context) (map[string]interface{}, error) {
	if p.cfg.PublicKey == "" || p.cfg.PrivateKey == "" {
		return nil, fmt.Errorf("webpush: VAPID keys not configured")
	}
	return map[string]interface{}{"kind": "webpush", "vapid_public_key": p.cfg.PublicKey, "subject": p.cfg.Subject}, nil
}

// GenerateVAPIDKeys creates a new VAPID key pair (base64url, as browsers expect).
func GenerateVAPIDKeys() (publicKey, privateKey string, err error) {
	privateKey, publicKey, err = webpush.GenerateVAPIDKeys()
	return publicKey, privateKey, err
}
