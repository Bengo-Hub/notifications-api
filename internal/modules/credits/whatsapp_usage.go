package credits

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/uuid"
)

// WhatsAppUsageCategory is one tenant's movement in one WhatsApp credit wallet over a range, as
// subscriptions-api reports it (GET /api/v1/s2s/credits/whatsapp-usage). Service is the wallet:
// a category service, or whatsapp_quota for the retired tiers' shared allowance.
type WhatsAppUsageCategory struct {
	Service          string  `json:"service"`
	MetaCategory     string  `json:"meta_category,omitempty"`
	UnitsBought      int64   `json:"units_bought"`
	KESPaid          float64 `json:"kes_paid"`
	UnitsConsumed    int64   `json:"units_consumed"`
	OverageKES       float64 `json:"overage_kes"`
	UnitPriceKES     float64 `json:"unit_price_kes"`
	ConsumedValueKES float64 `json:"consumed_value_kes"`
}

// WhatsAppUsageTenant groups one tenant's categories.
type WhatsAppUsageTenant struct {
	TenantID   uuid.UUID               `json:"tenant_id"`
	Slug       string                  `json:"slug"`
	Name       string                  `json:"name"`
	Categories []WhatsAppUsageCategory `json:"categories"`
}

// WhatsAppUsage is every tenant's WhatsApp purchases and spends for whole UTC days from..to.
type WhatsAppUsage struct {
	From     string                `json:"from"`
	To       string                `json:"to"`
	Currency string                `json:"currency"`
	Tenants  []WhatsAppUsageTenant `json:"tenants"`
}

// WhatsAppUsage reads per tenant WhatsApp credits bought and spent for the platform cost report.
// from and to are YYYY-MM-DD, both inclusive.
func (c *Client) WhatsAppUsage(ctx context.Context, from, to string) (WhatsAppUsage, error) {
	q := url.Values{}
	q.Set("from", from)
	q.Set("to", to)
	resp, err := c.sc.Get(ctx, "/api/v1/s2s/credits/whatsapp-usage?"+q.Encode(), c.headers())
	if err != nil {
		return WhatsAppUsage{}, fmt.Errorf("whatsapp usage: %w", err)
	}
	if !resp.IsSuccess() {
		return WhatsAppUsage{}, fmt.Errorf("whatsapp usage: status %d", resp.StatusCode)
	}
	var out WhatsAppUsage
	if err := resp.DecodeJSON(&out); err != nil {
		return WhatsAppUsage{}, fmt.Errorf("whatsapp usage decode: %w", err)
	}
	return out, nil
}
