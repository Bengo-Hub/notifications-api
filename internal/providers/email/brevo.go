package email

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const brevoEndpoint = "https://api.brevo.com/v3/smtp/email"

var providerHTTPClient = &http.Client{Timeout: 15 * time.Second}

// BrevoConfig holds Brevo (ex-Sendinblue) configuration.
type BrevoConfig struct {
	APIKey      string
	SenderName  string
	SenderEmail string
}

// BrevoProvider sends transactional emails via Brevo's SMTP API.
type BrevoProvider struct {
	cfg BrevoConfig
}

func NewBrevoProvider(cfg BrevoConfig) *BrevoProvider {
	return &BrevoProvider{cfg: cfg}
}

func (p *BrevoProvider) Name() string { return "brevo" }

func (p *BrevoProvider) SendEmail(ctx context.Context, from string, to []string, cc []string, bcc []string, replyTo string, subject string, htmlBody string, textBody string, attachments []Attachment) error {
	if p.cfg.APIKey == "" {
		return fmt.Errorf("brevo: api key not configured")
	}

	senderEmail := from
	if senderEmail == "" {
		senderEmail = p.cfg.SenderEmail
	}
	if senderEmail == "" {
		return fmt.Errorf("brevo: sender email not configured")
	}

	senderName := p.cfg.SenderName
	if senderName == "" {
		senderName = "Notifications"
	}

	recipients := make([]brevoRecipient, 0, len(to))
	for _, addr := range to {
		recipients = append(recipients, brevoRecipient{Email: addr})
	}

	var ccRecipients []brevoRecipient
	for _, addr := range cc {
		ccRecipients = append(ccRecipients, brevoRecipient{Email: addr})
	}

	var bccRecipients []brevoRecipient
	for _, addr := range bcc {
		bccRecipients = append(bccRecipients, brevoRecipient{Email: addr})
	}

	payload := brevoPayload{
		Sender:      brevoSender{Name: senderName, Email: senderEmail},
		To:          recipients,
		Cc:          ccRecipients,
		Bcc:         bccRecipients,
		Subject:     subject,
		HTMLContent: htmlBody,
	}
	if replyTo != "" {
		payload.ReplyTo = &brevoRecipient{Email: replyTo}
	}
	if textBody != "" && htmlBody == "" {
		payload.TextContent = textBody
		payload.HTMLContent = ""
	}
	for _, att := range attachments {
		if len(att.Content) == 0 {
			continue
		}
		payload.Attachment = append(payload.Attachment, brevoAttachment{
			Name:    att.Filename,
			Content: base64.StdEncoding.EncodeToString(att.Content),
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("brevo: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brevoEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("brevo: create request: %w", err)
	}
	req.Header.Set("api-key", p.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("brevo: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("brevo: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

type brevoPayload struct {
	Sender      brevoSender       `json:"sender"`
	To          []brevoRecipient  `json:"to"`
	Cc          []brevoRecipient  `json:"cc,omitempty"`
	Bcc         []brevoRecipient  `json:"bcc,omitempty"`
	ReplyTo     *brevoRecipient   `json:"replyTo,omitempty"`
	Subject     string            `json:"subject"`
	HTMLContent string            `json:"htmlContent,omitempty"`
	TextContent string            `json:"textContent,omitempty"`
	Attachment  []brevoAttachment `json:"attachment,omitempty"`
}

type brevoAttachment struct {
	Content string `json:"content"` // base64-encoded file content
	Name    string `json:"name"`
}

type brevoSender struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type brevoRecipient struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}
