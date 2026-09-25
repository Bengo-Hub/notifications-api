package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// subscription builds a browser-style PushSubscription JSON pointing at endpoint.
func subscription(t *testing.T, endpoint string) string {
	t.Helper()
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	b, _ := json.Marshal(map[string]any{
		"endpoint": endpoint,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(auth),
		},
	})
	return string(b)
}

func newWebPush(t *testing.T) *WebPushProvider {
	t.Helper()
	pub, priv, err := GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	return NewWebPush(WebPushConfig{PublicKey: pub, PrivateKey: priv, Subject: "https://example.com"})
}

func TestWebPush_DeliversAndReportsGoneSubscriptions(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "vapid ") {
			t.Errorf("missing VAPID authorization header")
		}
		if r.Header.Get("Content-Encoding") != "aes128gcm" {
			t.Errorf("payload must be encrypted (aes128gcm)")
		}
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	live := subscription(t, srv.URL+"/live")
	gone := subscription(t, srv.URL+"/gone")
	err := newWebPush(t).SendPush(context.Background(), []string{live, gone, "fcm-token-not-mine"}, "New delivery job", "Order DL-1", map[string]string{"url": "/demo/active"})

	var dead *UnregisteredTokensError
	if !errors.As(err, &dead) {
		t.Fatalf("expected the gone subscription reported, got %v", err)
	}
	if len(dead.Tokens) != 1 || dead.Tokens[0] != gone || dead.Delivered != 1 {
		t.Fatalf("unexpected result: %+v", dead)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("FCM tokens must be skipped by the webpush provider, hits=%d", hits)
	}
}

func TestRouter_SplitsByTokenKind(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	r := &Router{Web: newWebPush(t)} // no Firebase project configured
	if err := r.SendPush(context.Background(), []string{subscription(t, srv.URL), "fcm-token"}, "t", "b", nil); err != nil {
		t.Fatalf("browser subscription should be delivered, FCM token left alone: %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected one Web Push delivery, got %d", hits)
	}
	if err := r.SendPush(context.Background(), []string{"fcm-token"}, "t", "b", nil); err == nil {
		t.Fatalf("a device no configured sender can reach must be an error, not a silent success")
	}
}
