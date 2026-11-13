package providers

import (
	"context"
	"fmt"
	"strings"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// WhatsAppCostAccount is one whatsapp/meta_cloud row we send from, for the platform cost report.
type WhatsAppCostAccount struct {
	TenantID      string
	IsPlatform    bool
	WABAID        string
	PhoneNumberID string
	// AccessToken is the resolved token: the platform's for the platform row and for a managed
	// link (TokenSource "platform"), else the row's own (empty when none is saved).
	AccessToken string
	TokenSource string
	APIVersion  string
}

// WhatsAppCostAccounts lists the platform tenant's own WhatsApp account (with the platform Meta
// token) and every tenant row holding a waba_id, each with its token resolved by the managed link
// rule (tenantWhatsAppSettings).
func (m *Manager) WhatsAppCostAccounts(ctx context.Context) ([]WhatsAppCostAccount, error) {
	var out []WhatsAppCostAccount
	plat, err := m.LoadPlatformMetaCredentials(ctx)
	if err == nil && strings.TrimSpace(plat["waba_id"]) != "" {
		out = append(out, WhatsAppCostAccount{
			TenantID: m.PlatformID, IsPlatform: true,
			WABAID:        strings.TrimSpace(plat["waba_id"]),
			PhoneNumberID: strings.TrimSpace(plat["phone_number_id"]),
			AccessToken:   strings.TrimSpace(plat["access_token"]),
			TokenSource:   "platform",
			APIVersion:    strings.TrimSpace(plat["api_version"]),
		})
	}
	ids, err := pcfg.ListTenantsWithSetting(ctx, m.dbCfg, m.env, "whatsapp", "meta_cloud", "waba_id")
	if err != nil {
		return nil, fmt.Errorf("list WhatsApp accounts: %w", err)
	}
	for _, id := range ids {
		if id == "" || strings.EqualFold(id, m.PlatformID) {
			continue
		}
		// The same token rule as sending: tenantWhatsAppSettings gives a managed link
		// (token_source=platform) the platform token, and every other row only its own.
		s, err := m.tenantWhatsAppSettings(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("load WhatsApp settings of tenant %s: %w", id, err)
		}
		a := WhatsAppCostAccount{
			TenantID:      id,
			WABAID:        strings.TrimSpace(s["waba_id"]),
			PhoneNumberID: strings.TrimSpace(s["phone_number_id"]),
			AccessToken:   strings.TrimSpace(s["access_token"]),
			TokenSource:   "own",
			APIVersion:    strings.TrimSpace(s["api_version"]),
		}
		if strings.TrimSpace(s[TokenSourceKey]) == TokenSourcePlatform {
			a.TokenSource = TokenSourcePlatform
		}
		if a.WABAID != "" {
			out = append(out, a)
		}
	}
	return out, nil
}
