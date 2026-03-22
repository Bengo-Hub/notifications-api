package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	StartTLS bool
}

type SMTPProvider struct {
	cfg SMTPConfig
}

func NewSMTPProvider(cfg SMTPConfig) *SMTPProvider {
	return &SMTPProvider{cfg: cfg}
}

func (p *SMTPProvider) Name() string { return "smtp" }

// extractEmail returns the bare email from "Name <email>" or just "email".
func extractEmail(s string) string {
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j >= 0 {
			return s[i+1 : i+j]
		}
	}
	return strings.TrimSpace(s)
}

func (p *SMTPProvider) SendEmail(ctx context.Context, from string, to []string, subject string, htmlBody string, textBody string) error {
	if from == "" {
		from = p.cfg.From
	}
	if p.cfg.Host == "" || p.cfg.Port == 0 {
		return fmt.Errorf("smtp not configured")
	}
	addr := fmt.Sprintf("%s:%d", p.cfg.Host, p.cfg.Port)
	auth := smtp.PlainAuth("", p.cfg.Username, p.cfg.Password, p.cfg.Host)

	// SMTP envelope requires bare email; headers can have display name
	envelopeFrom := extractEmail(from)

	// Normalize bare \n to \r\n for RFC 5321 compliance (Gmail rejects bare LF)
	normalizeCRLF := func(s string) string {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\n", "\r\n")
		return s
	}
	htmlBody = normalizeCRLF(htmlBody)
	textBody = normalizeCRLF(textBody)

	// Build RFC 5322 message
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + strings.Join(to, ",") + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	if htmlBody != "" && textBody != "" {
		b.WriteString("Content-Type: multipart/alternative; boundary=BOUNDARY\r\n\r\n")
		b.WriteString("--BOUNDARY\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + textBody + "\r\n")
		b.WriteString("--BOUNDARY\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + htmlBody + "\r\n--BOUNDARY--")
	} else if htmlBody != "" {
		b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n" + htmlBody)
	} else {
		b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n" + textBody)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	domain := p.cfg.Host

	// Always use manual SMTP client to control EHLO hostname (smtp.SendMail uses "localhost" which Gmail rejects)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	c, err := smtp.NewClient(conn, domain)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Quit()

	if err := c.Hello(domain); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err = c.StartTLS(&tls.Config{ServerName: domain}); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	}

	if p.cfg.Username != "" {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := c.Mail(envelopeFrom); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp rcpt to: %w", err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	return w.Close()
}
