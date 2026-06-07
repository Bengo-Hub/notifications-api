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

const sendGridEndpoint = "https://api.sendgrid.com/v3/mail/send"

var providerHTTPClient = &http.Client{Timeout: 15 * time.Second}

// SendWithSendGrid sends an email using the SendGrid v3 HTTP API.
func SendWithSendGrid(ctx context.Context, apiKey, from string, to []string, cc []string, bcc []string, subject, htmlBody, textBody string, attachments []Attachment) error {
	if apiKey == "" {
		return fmt.Errorf("sendgrid api key not configured")
	}

	p := sgPersonalization{To: make([]sgEmail, 0, len(to))}
	for _, addr := range to {
		p.To = append(p.To, sgEmail{Email: addr})
	}
	for _, addr := range cc {
		p.Cc = append(p.Cc, sgEmail{Email: addr})
	}
	for _, addr := range bcc {
		p.Bcc = append(p.Bcc, sgEmail{Email: addr})
	}
	personalizations := []sgPersonalization{p}

	content := make([]sgContent, 0, 2)
	if textBody != "" {
		content = append(content, sgContent{Type: "text/plain", Value: textBody})
	}
	if htmlBody != "" {
		content = append(content, sgContent{Type: "text/html", Value: htmlBody})
	}
	if len(content) == 0 {
		return fmt.Errorf("sendgrid: no content provided")
	}

	payload := sgPayload{
		Personalizations: personalizations,
		From:             sgEmail{Email: from},
		Subject:          subject,
		Content:          content,
	}
	for _, att := range attachments {
		if len(att.Content) == 0 {
			continue
		}
		ct := att.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		payload.Attachments = append(payload.Attachments, sgAttachment{
			Content:     base64.StdEncoding.EncodeToString(att.Content),
			Filename:    att.Filename,
			Type:        ct,
			Disposition: "attachment",
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sendgrid: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sendGridEndpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sendgrid: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := providerHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("sendgrid: send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sendgrid: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

type sgPayload struct {
	Personalizations []sgPersonalization `json:"personalizations"`
	From             sgEmail             `json:"from"`
	Subject          string              `json:"subject"`
	Content          []sgContent         `json:"content"`
	Attachments      []sgAttachment      `json:"attachments,omitempty"`
}

type sgAttachment struct {
	Content     string `json:"content"` // base64-encoded
	Filename    string `json:"filename"`
	Type        string `json:"type,omitempty"`
	Disposition string `json:"disposition,omitempty"`
}

type sgPersonalization struct {
	To  []sgEmail `json:"to"`
	Cc  []sgEmail `json:"cc,omitempty"`
	Bcc []sgEmail `json:"bcc,omitempty"`
}

type sgEmail struct {
	Email string `json:"email"`
}

type sgContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}
