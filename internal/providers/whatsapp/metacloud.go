package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// MetaCloudProvider implements WhatsAppProvider using the official Meta WhatsApp
// Cloud API (graph.facebook.com). It is the preferred WhatsApp provider: direct
// Meta pricing has no BSP per-message markup, and it is Meta-hosted (no reseller
// "instance" to keep connected like APIWap). APIWap remains as a fallback.
//
// Credentials are supplied via ProviderSetting (channel=whatsapp, provider=meta_cloud):
//   - access_token    (permanent system-user token) — REQUIRED
//   - phone_number_id  (the WhatsApp Business phone number ID) — REQUIRED
//   - api_version      (optional, defaults to v20.0)
//
// No secrets are hardcoded here; everything comes from config rows / env.
type MetaCloudProvider struct {
	accessToken   string
	phoneNumberID string
	apiVersion    string
	httpClient    *http.Client
}

type MetaCloudConfig struct {
	AccessToken   string
	PhoneNumberID string
	APIVersion    string
}

func NewMetaCloudProvider(cfg MetaCloudConfig) *MetaCloudProvider {
	version := cfg.APIVersion
	if version == "" {
		version = "v20.0"
	}
	return &MetaCloudProvider{
		accessToken:   cfg.AccessToken,
		phoneNumberID: cfg.PhoneNumberID,
		apiVersion:    version,
		httpClient:    &http.Client{Timeout: 15 * time.Second},
	}
}

func (p *MetaCloudProvider) Name() string {
	return "meta_cloud"
}

// SendWhatsApp sends a text-body WhatsApp message to each recipient via the
// Cloud API. metadata may carry "preview_url" (bool) for link previews.
func (p *MetaCloudProvider) SendWhatsApp(ctx context.Context, from string, to []string, body string, metadata map[string]interface{}) error {
	if p.accessToken == "" || p.phoneNumberID == "" {
		return fmt.Errorf("meta_cloud not configured")
	}
	previewURL := false
	if v, ok := metadata["preview_url"].(bool); ok {
		previewURL = v
	}
	for _, recipient := range to {
		if err := p.sendSingle(ctx, recipient, body, previewURL); err != nil {
			return err
		}
	}
	return nil
}

func (p *MetaCloudProvider) sendSingle(ctx context.Context, to, body string, previewURL bool) error {
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages", p.apiVersion, p.phoneNumberID)

	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "text",
		"text": map[string]interface{}{
			"preview_url": previewURL,
			"body":        body,
		},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("meta_cloud error: status %d", resp.StatusCode)
	}
	return nil
}
