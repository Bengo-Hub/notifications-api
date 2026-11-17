package handlers

import (
	"sort"
	"strings"

	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/ent"
)

// inheritableAccount describes a shared account a tenant uses from the platform tier until it
// brings its own, as resolved in internal/providers/provider_scope.go: the tenant owns the account
// once it has its own accountKey value; until then every send uses the platform's account, and the
// tenant may only set the presentation keys (sender address or ID) on top of it.
type inheritableAccount struct {
	accountKey   string
	presentation []string
}

// inheritableAccounts are the providers whose platform settings a tenant inherits. WhatsApp is not
// one: a tenant without its own number is never sent through the platform tier's
// (LoadTenantOnlyProviderSettings).
var inheritableAccounts = map[string]inheritableAccount{
	"email/smtp":         {accountKey: "host", presentation: []string{"from", "reply_to"}},
	"email/brevo":        {accountKey: "api_key", presentation: []string{"sender_email", "sender_name", "from", "reply_to"}},
	"sms/africastalking": {accountKey: "api_key", presentation: []string{"from"}},
}

// settingRow is one saved provider setting, reduced to what the inheritance view needs.
type settingRow struct {
	Key     string
	Value   string
	Secret  bool
	Managed bool // platform-managed: forced on every tenant
}

func settingRows(rows []*ent.ProviderSetting) []settingRow {
	out := make([]settingRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, settingRow{
			Key:     r.Key,
			Value:   r.Value,
			Secret:  r.IsSecret || r.IsEncrypted || encryption.IsSecret(r.Key),
			Managed: r.IsPlatformManaged,
		})
	}
	return out
}

// inheritedSettings is what a tenant without its own account sends with.
type inheritedSettings struct {
	// From is "platform" when the tenant uses the platform's account, "" when it has its own (or
	// there is nothing to inherit).
	From string
	// Effective holds the non-secret values in force: the platform's, with the tenant's own
	// presentation values on top. Never a password or API key.
	Effective map[string]string
	// Keys are the keys of Effective whose value comes from the platform.
	Keys []string
}

// inheritedProviderSettings works out whether the tenant's provider settings come from the
// platform tier and which values are in force. reveal shows the platform account's identity (SMTP
// host, port, username, SMS username); without it, which is every caller but a platform owner, only
// the presentation values are returned, so a tenant never reads the platform's account details.
func inheritedProviderSettings(providerType, providerName string, tenantRows, platformRows []settingRow, reveal bool) inheritedSettings {
	acct, ok := inheritableAccounts[providerType+"/"+providerName]
	if !ok {
		return inheritedSettings{}
	}
	own := map[string]string{}
	for _, r := range tenantRows {
		if v := strings.TrimSpace(r.Value); v != "" && !strings.HasPrefix(r.Key, "_") {
			own[r.Key] = v
		}
	}
	if v := own[acct.accountKey]; v != "" {
		forced := ""
		for _, r := range platformRows {
			if r.Managed && r.Key == acct.accountKey {
				forced = strings.TrimSpace(r.Value)
			}
		}
		if forced == "" || forced == v {
			return inheritedSettings{} // the tenant's own account is in force
		}
	}

	presentation := map[string]bool{}
	for _, k := range acct.presentation {
		presentation[k] = true
	}
	effective := map[string]string{}
	keys := []string{}
	configured := false
	for _, r := range platformRows {
		v := strings.TrimSpace(r.Value)
		if v == "" || strings.HasPrefix(r.Key, "_") {
			continue
		}
		configured = true
		if r.Secret {
			continue
		}
		if presentation[r.Key] && own[r.Key] != "" && !r.Managed {
			continue // the tenant's own sender value, added below
		}
		if !presentation[r.Key] && !reveal {
			continue
		}
		effective[r.Key] = v
		keys = append(keys, r.Key)
	}
	if !configured {
		return inheritedSettings{} // nothing saved at platform level to inherit
	}
	for k := range presentation {
		if _, set := effective[k]; !set && own[k] != "" {
			effective[k] = own[k]
		}
	}
	sort.Strings(keys)
	return inheritedSettings{From: "platform", Effective: effective, Keys: keys}
}
