package email

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

// loginAuth implements smtp.Auth for the AUTH LOGIN mechanism, required by
// Microsoft 365 / Outlook which does not accept AUTH PLAIN.
type loginAuth struct{ username, password string }

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimRight(string(fromServer), ": ")) {
	case "username":
		return []byte(a.username), nil
	case "password":
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("smtp login auth: unexpected challenge %q", fromServer)
	}
}

type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	StartTLS bool
	SSL      bool // implicit TLS (port 465) — mutually exclusive with StartTLS
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

func (p *SMTPProvider) SendEmail(ctx context.Context, from string, to []string, cc []string, subject string, htmlBody string, textBody string) error {
	if from == "" {
		from = p.cfg.From
	} else if !strings.Contains(from, "@") && p.cfg.From != "" {
		// "from" is just a display name (e.g. "Urban Loft Cafe") — wrap with configured email
		from = fmt.Sprintf("%s <%s>", from, extractEmail(p.cfg.From))
	}
	if p.cfg.Host == "" || p.cfg.Port == 0 {
		return fmt.Errorf("smtp not configured")
	}
	addr := fmt.Sprintf("%s:%d", p.cfg.Host, p.cfg.Port)

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
	if len(cc) > 0 {
		b.WriteString("Cc: " + strings.Join(cc, ",") + "\r\n")
	}
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

	// Implicit TLS (SSL) when port is 465 or SSL flag is set; otherwise plain + STARTTLS.
	useSSL := p.cfg.SSL || p.cfg.Port == 465
	var conn net.Conn
	if useSSL {
		tlsConn, tlsErr := tls.Dial("tcp", addr, &tls.Config{ServerName: domain})
		if tlsErr != nil {
			return fmt.Errorf("smtp dial: %w", tlsErr)
		}
		conn = tlsConn
	} else {
		plainConn, plainErr := net.Dial("tcp", addr)
		if plainErr != nil {
			return fmt.Errorf("smtp dial: %w", plainErr)
		}
		conn = plainConn
	}
	c, err := smtp.NewClient(conn, domain)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Quit()

	if err := c.Hello(domain); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}

	// STARTTLS only needed for non-SSL connections (ports 587, 25)
	if !useSSL {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err = c.StartTLS(&tls.Config{ServerName: domain}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}

	if p.cfg.Username != "" {
		// Choose AUTH method based on what the server advertises.
		// Microsoft 365 / Outlook only supports LOGIN; most others support both.
		var auth smtp.Auth
		if _, params := c.Extension("AUTH"); strings.Contains(strings.ToUpper(params), "LOGIN") {
			auth = &loginAuth{p.cfg.Username, p.cfg.Password}
		} else {
			auth = smtp.PlainAuth("", p.cfg.Username, p.cfg.Password, p.cfg.Host)
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := c.Mail(envelopeFrom); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	allRcpts := append(to, cc...)
	for _, rcpt := range allRcpts {
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
