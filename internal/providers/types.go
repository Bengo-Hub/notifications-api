package providers

import "context"

// EmailProvider sends email messages.
type EmailProvider interface {
	SendEmail(ctx context.Context, from string, to []string, cc []string, subject string, htmlBody string, textBody string) error
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
