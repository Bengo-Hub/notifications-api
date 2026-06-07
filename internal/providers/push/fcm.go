package push

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var providerHTTPClient = &http.Client{Timeout: 15 * time.Second}

// FCMConfig holds the Firebase Cloud Messaging configuration.
type FCMConfig struct {
	// ProjectID is the Firebase project ID. If empty it is extracted from ServiceAccount JSON.
	ProjectID string
	// ServiceAccount is the raw JSON content of a Firebase service account key file.
	ServiceAccount string
}

type serviceAccountKey struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

// FCMProvider sends push notifications via Firebase Cloud Messaging HTTP v1 API.
type FCMProvider struct {
	cfg   FCMConfig
	saKey *serviceAccountKey
}

// NewFCM creates a new FCM push provider. ServiceAccount must be the raw JSON of a service account key.
func NewFCM(cfg FCMConfig) *FCMProvider {
	p := &FCMProvider{cfg: cfg}
	if cfg.ServiceAccount != "" {
		var key serviceAccountKey
		if err := json.Unmarshal([]byte(cfg.ServiceAccount), &key); err == nil {
			p.saKey = &key
			if p.cfg.ProjectID == "" {
				p.cfg.ProjectID = key.ProjectID
			}
		}
	}
	return p
}

func (p *FCMProvider) Name() string { return "fcm" }

// SendPush sends a push notification to all provided device tokens.
func (p *FCMProvider) SendPush(ctx context.Context, tokens []string, title, body string, data map[string]string) error {
	if p.cfg.ProjectID == "" {
		return fmt.Errorf("fcm: project_id not configured")
	}
	if p.saKey == nil {
		return fmt.Errorf("fcm: service account not configured")
	}
	if len(tokens) == 0 {
		return nil
	}

	accessToken, err := p.getAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("fcm: get access token: %w", err)
	}

	var lastErr error
	for _, token := range tokens {
		if err := p.sendMessage(ctx, accessToken, token, title, body, data); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// getAccessToken exchanges a service-account JWT for a Google OAuth2 access token.
func (p *FCMProvider) getAccessToken(ctx context.Context) (string, error) {
	block, _ := pem.Decode([]byte(p.saKey.PrivateKey))
	if block == nil {
		return "", fmt.Errorf("fcm: failed to decode PEM block from private key")
	}
	rawKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("fcm: parse private key: %w", err)
	}
	rsaKey, ok := rawKey.(*rsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("fcm: expected RSA private key, got %T", rawKey)
	}

	tokenURI := p.saKey.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   p.saKey.ClientEmail,
		"sub":   p.saKey.ClientEmail,
		"aud":   tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
	}
	jwtToken := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := jwtToken.SignedString(rsaKey)
	if err != nil {
		return "", fmt.Errorf("fcm: sign jwt: %w", err)
	}

	formData := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signed},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm: token request: %w", err)
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("fcm: decode token response: %w", err)
	}
	if tokenResp.Error != "" {
		return "", fmt.Errorf("fcm: token error: %s — %s", tokenResp.Error, tokenResp.ErrorDesc)
	}
	return tokenResp.AccessToken, nil
}

type fcmMessage struct {
	Message fcmMessageBody `json:"message"`
}

type fcmMessageBody struct {
	Token        string            `json:"token"`
	Notification *fcmNotification  `json:"notification,omitempty"`
	Data         map[string]string `json:"data,omitempty"`
}

type fcmNotification struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// sendMessage sends one FCM message to a single device token.
func (p *FCMProvider) sendMessage(ctx context.Context, accessToken, token, title, body string, data map[string]string) error {
	msg := fcmMessage{}
	msg.Message.Token = token
	if title != "" || body != "" {
		msg.Message.Notification = &fcmNotification{Title: title, Body: body}
	}
	if len(data) > 0 {
		msg.Message.Data = data
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.cfg.ProjectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("fcm: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fcm: send failed (status %d): %s", resp.StatusCode, string(b))
	}
	return nil
}
