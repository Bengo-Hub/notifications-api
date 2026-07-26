package messaging

import "time"

// SenderScope determines which provider to use for sending.
const (
	SenderScopePlatform = "platform" // Use platform provider (notifications@codevertexafrica.com)
	SenderScopeTenant   = "tenant"   // Use tenant's configured provider, fallback to platform
)

// RecipientTarget describes who should receive the notification.
const (
	TargetCustomer      = "customer"       // End customer (order updates, receipts)
	TargetTenantAdmin   = "tenant_admin"   // Tenant administrators (subscription, stock alerts)
	TargetStaff         = "staff"          // Tenant staff (ticket assignments, inventory alerts)
	TargetRider         = "rider"          // Delivery rider (task assignments)
	TargetPlatformAdmin = "platform_admin" // Codevertex platform admins
)

// Message represents a tenant-scoped notification request.
type Message struct {
	TenantID       string         `json:"tenantId"`
	Channel        string         `json:"channel"`      // email | sms | push | whatsapp
	TemplateID     string         `json:"template"`      // e.g. email/payment_success
	SenderScope    string         `json:"senderScope"`   // "platform" or "tenant" (default: tenant)
	Target         string         `json:"target"`        // recipient target type
	To             []string       `json:"to"`
	Cc             []string       `json:"cc,omitempty"`  // email CC recipients (email channel only)
	Bcc            []string       `json:"bcc,omitempty"` // email BCC recipients (email channel only) — e.g. tenant copy of a new-order email
	Data           map[string]any `json:"data"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Attachments    []Attachment   `json:"attachments,omitempty"` // email channel only; optional
	RequestID      string         `json:"requestId"`
	IdempotencyKey string         `json:"idempotencyKey"`
	QueuedAt       time.Time      `json:"queuedAt"`
}

// Attachment is an optional email file attachment carried on a Message. Content is raw
// bytes (JSON-encoded as base64), so producers like the ERP can send base64 directly.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	Content     []byte `json:"content"`
}

// EffectiveSenderScope returns the sender scope, defaulting to tenant if empty.
func (m *Message) EffectiveSenderScope() string {
	if m.SenderScope == "" {
		return SenderScopeTenant
	}
	return m.SenderScope
}
