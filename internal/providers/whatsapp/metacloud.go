package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// SendWhatsApp sends a WhatsApp message to each recipient via the Cloud API.
//
// Meta only allows a free-form "text" message within an active 24h customer-service window (the
// recipient messaged the business first, or replied within the last 24h) — any business-initiated
// message outside that window (which is the overwhelming majority of this platform's use cases:
// order confirmations, invoice notices, OTPs, reminders) MUST use a pre-approved message template
// instead, or Meta rejects it outright. metadata therefore supports two modes:
//   - metadata["template_name"] set (string): sends a template message. metadata["template_language"]
//     (default "en_US") and metadata["template_params"] ([]string, substituted in order into the
//     template's body placeholders {{1}}, {{2}}, ...) configure it.
//   - metadata["template_name"] absent: falls back to the original free-form text body (only valid
//     inside an open reply window). metadata["preview_url"] (bool) still applies to that path.
func (p *MetaCloudProvider) SendWhatsApp(ctx context.Context, from string, to []string, body string, metadata map[string]interface{}) error {
	if p.accessToken == "" || p.phoneNumberID == "" {
		return fmt.Errorf("meta_cloud not configured")
	}
	templateName, _ := metadata["template_name"].(string)
	for _, recipient := range to {
		var err error
		if templateName != "" {
			language, _ := metadata["template_language"].(string)
			if language == "" {
				language = "en_US"
			}
			var params []string
			if raw, ok := metadata["template_params"].([]string); ok {
				params = raw
			} else if raw, ok := metadata["template_params"].([]interface{}); ok {
				for _, v := range raw {
					if s, ok := v.(string); ok {
						params = append(params, s)
					}
				}
			}
			err = p.sendTemplate(ctx, recipient, templateName, language, params)
		} else {
			previewURL := false
			if v, ok := metadata["preview_url"].(bool); ok {
				previewURL = v
			}
			err = p.sendText(ctx, recipient, body, previewURL)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *MetaCloudProvider) sendText(ctx context.Context, to, body string, previewURL bool) error {
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
	return p.post(ctx, payload)
}

// sendTemplate sends a pre-approved WhatsApp message template (the only way to reach a recipient
// outside an active 24h reply window). params are substituted positionally into the template
// body's {{1}}, {{2}}, ... placeholders — Meta does its own server-side rendering from the
// template it already has on file, so no local template body is sent here, only the name +
// language + ordered parameter values.
func (p *MetaCloudProvider) sendTemplate(ctx context.Context, to, templateName, language string, params []string) error {
	components := []map[string]interface{}{}
	if len(params) > 0 {
		bodyParams := make([]map[string]interface{}, 0, len(params))
		for _, v := range params {
			bodyParams = append(bodyParams, map[string]interface{}{"type": "text", "text": v})
		}
		components = append(components, map[string]interface{}{"type": "body", "parameters": bodyParams})
	}
	template := map[string]interface{}{
		"name":     templateName,
		"language": map[string]interface{}{"code": language},
	}
	if len(components) > 0 {
		template["components"] = components
	}
	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "template",
		"template":          template,
	}
	return p.post(ctx, payload)
}

// AccountInfo confirms the configured credentials are valid by querying Meta's Graph API for the
// connected phone number's own status (verified display name, quality rating, platform type) —
// implements providers.AccountInfoProvider so "Test Connection" can confirm connectivity without
// sending a real WhatsApp message.
func (p *MetaCloudProvider) AccountInfo(ctx context.Context) (map[string]interface{}, error) {
	if p.accessToken == "" || p.phoneNumberID == "" {
		return nil, fmt.Errorf("meta_cloud not configured")
	}
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s?fields=verified_name,display_phone_number,quality_rating,platform_type,code_verification_status", p.apiVersion, p.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.accessToken)
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meta_cloud account info error: status %d: %s", resp.StatusCode, string(body))
	}
	var info map[string]interface{}
	if jerr := json.Unmarshal(body, &info); jerr != nil {
		return nil, fmt.Errorf("meta_cloud: unexpected account info response: %s", string(body))
	}
	return info, nil
}

func (p *MetaCloudProvider) post(ctx context.Context, payload map[string]interface{}) error {
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages", p.apiVersion, p.phoneNumberID)

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

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("meta_cloud error: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
