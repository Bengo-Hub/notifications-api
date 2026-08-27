package sms

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type AfricasTalkingConfig struct {
	Username string
	APIKey   string
	From     string
}

type africasTalkingProvider struct {
	cfg AfricasTalkingConfig
	cl  *http.Client
}

func NewAfricasTalking(cfg AfricasTalkingConfig) *africasTalkingProvider {
	return &africasTalkingProvider{
		cfg: cfg,
		cl:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *africasTalkingProvider) Name() string { return "africastalking" }

// apiHost picks the production vs. sandbox host. The sandbox app username is always literally
// "sandbox", which only exists on the separate api.sandbox.* host — a sandbox-configured provider
// hitting the production host fails auth regardless of what else is correct.
func (p *africasTalkingProvider) apiHost() string {
	if p.cfg.Username == "sandbox" {
		return "https://api.sandbox.africastalking.com"
	}
	return "https://api.africastalking.com"
}

func (p *africasTalkingProvider) SendSMS(ctx context.Context, from string, to []string, body string) error {
	if p.cfg.APIKey == "" || p.cfg.Username == "" {
		return fmt.Errorf("africastalking not configured")
	}
	if from == "" {
		from = p.cfg.From
	}
	form := url.Values{}
	form.Set("username", p.cfg.Username)
	form.Set("to", strings.Join(to, ","))
	form.Set("message", body)
	if from != "" {
		form.Set("from", from)
	}
	// Africa's Talking's /version1/messaging endpoint only accepts
	// application/x-www-form-urlencoded (confirmed directly against their live API — a
	// JSON body is rejected outright with 415 before auth is even checked, so this was
	// silently failing every real send).
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.apiHost()+"/version1/messaging", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("apiKey", p.cfg.APIKey)
	resp, err := p.cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("africastalking error: %s: %s", resp.Status, string(respBody))
	}
	// A 2xx here only means AT accepted the REQUEST — the actual per-recipient send result
	// is a separate field inside the body, and can independently fail (invalid/unregistered
	// sender id, insufficient account balance, blacklisted/DND number, invalid phone number,
	// etc.) while the HTTP call itself still returns 200. Previously this was never checked,
	// so every one of those failure modes was silently swallowed as a "successful" send.
	var parsed struct {
		SMSMessageData struct {
			Message    string `json:"Message"`
			Recipients []struct {
				Number     string `json:"number"`
				Status     string `json:"status"`
				StatusCode int    `json:"statusCode"`
				Cost       string `json:"cost"`
			} `json:"Recipients"`
		} `json:"SMSMessageData"`
	}
	if jerr := json.Unmarshal(respBody, &parsed); jerr != nil {
		// Response wasn't the shape we expect — don't claim success over an unparseable body.
		return fmt.Errorf("africastalking: unexpected response body: %s", string(respBody))
	}
	recipients := parsed.SMSMessageData.Recipients
	if len(recipients) == 0 {
		return fmt.Errorf("africastalking: no recipients in response (message: %q, raw: %s)", parsed.SMSMessageData.Message, string(respBody))
	}
	var failures []string
	for _, r := range recipients {
		// AT's documented success codes are 100/101/102; everything else (401 risky number,
		// 402 invalid sender id, 403/406 insufficient balance, 404 invalid phone number,
		// 405 unsupported number type, 407 blacklisted, 408 could not route, 409 do-not-
		// disturb rejection, 500/501/502 gateway errors, etc.) is a real failure.
		if r.StatusCode < 100 || r.StatusCode > 102 {
			failures = append(failures, fmt.Sprintf("%s: %s (code %d)", r.Number, r.Status, r.StatusCode))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("africastalking: %d/%d recipient(s) failed: %s", len(failures), len(recipients), strings.Join(failures, "; "))
	}
	return nil
}

// GetBalance queries Africa's Talking's real account wallet balance (GET /version1/user),
// parsing the "KES 110.0000"-style string AT returns into a plain float. Used to gate
// platform-scope sends (OTPs, admin alerts — never billed to a tenant's own credit wallet)
// directly against the real provider balance instead of a TenantCredit row.
func (p *africasTalkingProvider) GetBalance(ctx context.Context) (float64, error) {
	if p.cfg.APIKey == "" || p.cfg.Username == "" {
		return 0, fmt.Errorf("africastalking not configured")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.apiHost()+"/version1/user?username="+url.QueryEscape(p.cfg.Username), nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("apiKey", p.cfg.APIKey)
	resp, err := p.cl.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("africastalking balance check error: %s: %s", resp.Status, string(body))
	}
	var parsed struct {
		UserData struct {
			Balance string `json:"balance"`
		} `json:"UserData"`
	}
	if jerr := json.Unmarshal(body, &parsed); jerr != nil {
		return 0, fmt.Errorf("africastalking: unexpected balance response: %s", string(body))
	}
	// "KES 110.0000" -> 110.0000; fall back to parsing the whole string if there's no prefix.
	raw := parsed.UserData.Balance
	if fields := strings.Fields(raw); len(fields) == 2 {
		raw = fields[1]
	}
	bal, perr := strconv.ParseFloat(raw, 64)
	if perr != nil {
		return 0, fmt.Errorf("africastalking: could not parse balance %q", parsed.UserData.Balance)
	}
	return bal, nil
}
