package sms

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	// silently failing every real send). The sandbox app username is always literally
	// "sandbox", which only exists on the separate api.sandbox.* host — a sandbox-configured
	// provider hitting the production host would fail auth even with the fix above.
	apiHost := "https://api.africastalking.com"
	if p.cfg.Username == "sandbox" {
		apiHost = "https://api.sandbox.africastalking.com"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiHost+"/version1/messaging", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("apiKey", p.cfg.APIKey)
	resp, err := p.cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("africastalking error: %s", resp.Status)
	}
	return nil
}
