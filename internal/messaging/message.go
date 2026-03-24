package messaging

import "time"

// SenderScope determines which provider to use for sending.
const (
	SenderScopePlatform = "platform" // Use platform provider (notifications@codevertexitsolutions.com)
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
	Data           map[string]any `json:"data"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	RequestID      string         `json:"requestId"`
	IdempotencyKey string         `json:"idempotencyKey"`
	QueuedAt       time.Time      `json:"queuedAt"`
}

// EffectiveSenderScope returns the sender scope, defaulting to tenant if empty.
func (m *Message) EffectiveSenderScope() string {
	if m.SenderScope == "" {
		return SenderScopeTenant
	}
	return m.SenderScope
}
