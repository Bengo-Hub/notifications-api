package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
//     template's body placeholders {{1}}, {{2}}, ...) configure it. metadata["template_button_param"]
//     (string), when the template has a URL button with a dynamic suffix, supplies that suffix.
//     metadata["template_fallback_name"]/["template_fallback_params"], when set, name an
//     already-APPROVED template + its own param list to retry with on ANY failure of the primary
//     send — not only a button send. This is the standard way a newly-drafted template (polished
//     wording, possibly with a button) ships ahead of its one-time Meta sync/approval: the code and
//     the template registration deploy independently, and every send safely degrades to the old,
//     already-working template until Meta approves the new one.
//   - metadata["template_name"] absent, metadata["cta_button_text"]/["cta_button_url"] both set:
//     sends a freeform "interactive" cta_url message — body text plus one real tappable button
//     (no template registration/approval needed, only the same open-reply-window requirement
//     freeform text already has). Use this for a notification that always sends freeform and
//     shows a link — attach it as a button rather than embedding the raw URL in the text.
//   - metadata["template_name"] absent, no cta_button_*: falls back to the original free-form text
//     body. metadata["preview_url"] (bool) still applies to that path.
func (p *MetaCloudProvider) SendWhatsApp(ctx context.Context, from string, to []string, body string, metadata map[string]interface{}) error {
	if p.accessToken == "" || p.phoneNumberID == "" {
		return fmt.Errorf("meta_cloud not configured")
	}
	templateName, _ := metadata["template_name"].(string)
	dialCode, _ := metadata["default_dial_code"].(string)
	for _, raw := range to {
		recipient := internationalWhatsAppNumber(raw, dialCode)
		var err error
		if templateName != "" {
			language, _ := metadata["template_language"].(string)
			if language == "" {
				language = "en_US"
			}
			params := stringSliceParam(metadata["template_params"])
			buttonParam, _ := metadata["template_button_param"].(string)
			err = p.sendTemplate(ctx, recipient, templateName, language, params, buttonParam)
			// Retry with the fallback template on ANY primary-send failure, not just a button
			// send — the primary is often a newly-drafted template (polished wording, possibly
			// with a button) that hasn't cleared Meta review yet either, regardless of whether
			// it has a button. The fallback is expected to be an already-APPROVED template, so
			// this degrades a brand-new/pending template straight to today's working behavior
			// instead of failing the notification outright.
			if err != nil {
				if fallbackName, ok := metadata["template_fallback_name"].(string); ok && fallbackName != "" {
					fallbackParams := stringSliceParam(metadata["template_fallback_params"])
					err = p.sendTemplate(ctx, recipient, fallbackName, language, fallbackParams, "")
				}
			}
		} else if ctaText, _ := metadata["cta_button_text"].(string); ctaText != "" {
			if ctaURL, _ := metadata["cta_button_url"].(string); ctaURL != "" {
				err = p.sendInteractiveCTA(ctx, recipient, body, ctaText, ctaURL)
			} else {
				err = p.sendText(ctx, recipient, body, false)
			}
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

// stringSliceParam coerces a metadata value into a []string — it may already be []string (set
// directly by Go callers) or []interface{} (after a JSON round-trip through the NATS event bus).
func stringSliceParam(v interface{}) []string {
	if raw, ok := v.([]string); ok {
		return raw
	}
	var out []string
	if raw, ok := v.([]interface{}); ok {
		for _, item := range raw {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// sendInteractiveCTA sends a freeform (session-window) message with a real tappable "cta_url"
// button — Meta's interactive message type, distinct from a template's BUTTONS component and
// requiring no template registration/approval at all, only an active 24h customer-service window
// (the same requirement sendText already has). This is how a link gets attached as an action
// button on a send that, unlike the templated order-notification path, is never routed through a
// Meta-approved template — see pos/receipt_share's WhatsApp send for the one live example today.
func (p *MetaCloudProvider) sendInteractiveCTA(ctx context.Context, to, bodyText, buttonText, url string) error {
	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                to,
		"type":              "interactive",
		"interactive": map[string]interface{}{
			"type": "cta_url",
			"body": map[string]interface{}{"text": bodyText},
			"action": map[string]interface{}{
				"name": "cta_url",
				"parameters": map[string]interface{}{
					"display_text": buttonText,
					"url":          url,
				},
			},
		},
	}
	_, err := p.post(ctx, payload)
	return err
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
	_, err := p.post(ctx, payload)
	return err
}

// sendTemplate sends a pre-approved WhatsApp message template (the only way to reach a recipient
// outside an active 24h reply window). params are substituted positionally into the template
// body's {{1}}, {{2}}, ... placeholders — Meta does its own server-side rendering from the
// template it already has on file, so no local template body is sent here, only the name +
// language + ordered parameter values. buttonParam, when non-empty, fills the dynamic suffix of
// the template's (single, index-0) URL button — Meta requires a SEPARATE "button" component for
// this, scoped independently from the body's own {{1}}, {{2}}, ... numbering.
func (p *MetaCloudProvider) sendTemplate(ctx context.Context, to, templateName, language string, params []string, buttonParam string) error {
	components := []map[string]interface{}{}
	if len(params) > 0 {
		bodyParams := make([]map[string]interface{}, 0, len(params))
		for _, v := range params {
			bodyParams = append(bodyParams, map[string]interface{}{"type": "text", "text": v})
		}
		components = append(components, map[string]interface{}{"type": "body", "parameters": bodyParams})
	}
	if buttonParam != "" {
		components = append(components, map[string]interface{}{
			"type":     "button",
			"sub_type": "url",
			"index":    "0",
			"parameters": []map[string]interface{}{
				{"type": "text", "text": buttonParam},
			},
		})
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
	_, err := p.post(ctx, payload)
	return err
}

// normalizeWhatsAppNumber strips everything Meta's Cloud API doesn't accept in the "to" field —
// a leading "+" and any non-digit formatting characters (spaces, dashes, parens). Meta requires
// bare "countrycode+number" digits; a "+254..." recipient (the natural, common way a phone number
// gets typed or stored) is silently accepted by the send API (2xx) but never actually delivered,
// with no error surfaced anywhere in this call chain — confirmed live: "+254743793901" accepted
// but never arrived, "254743793901" (same number, no leading +) delivered immediately.
func normalizeWhatsAppNumber(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// defaultDialCode is used when a local number arrives without a country code and the sender did
// not pass the tenant's own (most tenants are in Kenya).
const defaultDialCode = "254"

// internationalWhatsAppNumber turns what customers actually type into the country-code form Meta
// delivers to. A local number ("0712 345 678", or "712345678" with the leading 0 dropped) gets the
// tenant's dial code; a number that already carries a country code ("+254...", "254...", "256...")
// is only stripped of formatting. Without this a local number was sent as "0712345678" and never
// arrived.
func internationalWhatsAppNumber(raw, dialCode string) string {
	digits := normalizeWhatsAppNumber(raw)
	if dialCode == "" {
		dialCode = defaultDialCode
	}
	dialCode = normalizeWhatsAppNumber(dialCode)
	switch {
	case strings.HasPrefix(digits, "00"):
		return digits[2:] // international prefix form, e.g. 00254...
	case len(digits) == 10 && strings.HasPrefix(digits, "0"):
		return dialCode + digits[1:]
	case len(digits) == 9 && (digits[0] == '7' || digits[0] == '1'):
		return dialCode + digits
	default:
		return digits
	}
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

// post returns Meta's raw response body on success so callers that need it (SendTextMessage, to
// capture the real wamid) can parse it; callers that don't just ignore it — zero behavior change
// for sendText/sendTemplate below.
func (p *MetaCloudProvider) post(ctx context.Context, payload map[string]interface{}) ([]byte, error) {
	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/messages", p.apiVersion, p.phoneNumberID)

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.accessToken)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("meta_cloud error: status %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// SendTextMessage sends free-form text (valid only within Meta's 24h customer-service window) and
// returns Meta's own message id (wamid...) from the response body's messages[0].id, so callers —
// the WhatsApp inbox reply path — can correlate a later delivery-status webhook back to this send.
func (p *MetaCloudProvider) SendTextMessage(ctx context.Context, to, body string) (string, error) {
	if p.accessToken == "" || p.phoneNumberID == "" {
		return "", fmt.Errorf("meta_cloud not configured")
	}
	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                normalizeWhatsAppNumber(to),
		"type":              "text",
		"text":              map[string]interface{}{"body": body},
	}
	respBody, err := p.post(ctx, payload)
	if err != nil {
		return "", err
	}
	var parsed struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if jerr := json.Unmarshal(respBody, &parsed); jerr == nil && len(parsed.Messages) > 0 {
		return parsed.Messages[0].ID, nil
	}
	return "", nil
}
