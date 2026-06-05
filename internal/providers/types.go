package providers

import (
	"context"

	"github.com/bengobox/notifications-api/internal/providers/email"
)

// EmailProvider sends email messages. attachments is optional (nil/empty = none);
// every provider accepts the same signature and includes attachments only when present.
type EmailProvider interface {
	SendEmail(ctx context.Context, from string, to []string, cc []string, bcc []string, subject string, htmlBody string, textBody string, attachments []email.Attachment) error
	Name() string
}

// SMSProvider sends SMS messages.
type SMSProvider interface {
	SendSMS(ctx context.Context, from string, to []string, body string) error
	Name() string
}

// WhatsAppProvider sends WhatsApp messages.
type WhatsAppProvider interface {
	SendWhatsApp(ctx context.Context, from string, to []string, body string, metadata map[string]interface{}) error
	Name() string
}

// PushProvider sends push notifications (FCM/APNS).
type PushProvider interface {
	SendPush(ctx context.Context, tokens []string, title, body string, data map[string]string) error
	Name() string
}
