package providers

import (
	"context"
	"strings"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

// Provider scoping. Shared accounts (the platform's SMTP server, Africa's Talking account, Brevo
// key, Firebase project) are configured once at platform level and used by every tenant that has
// not brought its own. A tenant that brings its own account uses it as a whole.
//
// Credentials are never mixed across tiers. Provider settings are stored key by key and the
// generic loader merges them key by key, so a tenant that saved only its own SMTP host used to
// inherit the platform's SMTP username and password, which were then sent to the tenant's server
// at login. Resolution below picks one tier for the account (host + credentials) and only lets a
// tenant override presentation values (sender address, sender ID) on the platform account.

// scopedSettings returns the settings a tenant saved itself and the platform's, separately.
// merged is the loader's key-by-key merge, used to honour platform-managed (forced) values.
func (m *Manager) scopedSettings(ctx context.Context, tenantID, channel, provider string) (tenantOwn, platform, merged pcfg.Settings) {
	merged, _ = pcfg.LoadTenantProviderSettings(ctx, m.dbCfg, tenantID, m.env, channel, provider, m.decryptionKey)
	platform, _ = pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, "platform", m.env, channel, provider, m.decryptionKey)
	if tenantID != "" && tenantID != "platform" {
		tenantOwn, _ = pcfg.LoadTenantOnlyProviderSettings(ctx, m.dbCfg, tenantID, m.env, channel, provider, m.decryptionKey)
	}
	if tenantOwn == nil {
		tenantOwn = pcfg.Settings{}
	}
	if platform == nil {
		platform = pcfg.Settings{}
	}
	if merged == nil {
		merged = pcfg.Settings{}
	}
	return tenantOwn, platform, merged
}

// ownsAccount reports whether the tenant's own value for the account's identifying key is the
// one in force (not overridden by a platform-managed value).
func ownsAccount(tenantOwn, merged pcfg.Settings, key string) bool {
	v := strings.TrimSpace(tenantOwn[key])
	return v != "" && strings.TrimSpace(merged[key]) == v
}

// smtpAccount resolves SMTP settings as one unit. envCfg supplies the platform's env fallback.
type smtpAccount struct {
	Host, Port, Username, Password, From, StartTLS, SSL string
	Own                                                 bool
}

func resolveSMTP(tenantOwn, platform, merged pcfg.Settings, env smtpAccount, dev bool) smtpAccount {
	if ownsAccount(tenantOwn, merged, "host") && (dev || !isLocalhost(tenantOwn["host"])) {
		return smtpAccount{
			Host:     tenantOwn["host"],
			Port:     firstNonEmpty(tenantOwn["port"], "587"),
			Username: tenantOwn["username"],
			Password: tenantOwn["password"],
			From:     firstNonEmpty(tenantOwn["from"], tenantOwn["username"]),
			StartTLS: tenantOwn["start_tls"],
			SSL:      tenantOwn["ssl"],
			Own:      true,
		}
	}
	return smtpAccount{
		Host:     firstNonEmpty(platform["host"], env.Host),
		Port:     firstNonEmpty(platform["port"], env.Port),
		Username: firstNonEmpty(platform["username"], env.Username),
		Password: firstNonEmpty(platform["password"], env.Password),
		// A tenant may send as its own address through the platform server (subject to the
		// server's sender policy); that is presentation, not a credential.
		From:     firstNonEmpty(tenantOwn["from"], platform["from"], env.From),
		StartTLS: firstNonEmpty(platform["start_tls"], env.StartTLS),
		SSL:      platform["ssl"],
	}
}

// resolveKeyedAccount resolves an API-key account (Africa's Talking, Brevo) as one unit: the
// tenant's own key with its own identity fields, else the platform's. presentation keys (sender
// ID / sender address / name) may still come from the tenant on the platform account.
func resolveKeyedAccount(tenantOwn, platform, merged pcfg.Settings, keyField string, identity, presentation []string, env map[string]string) (pcfg.Settings, bool) {
	out := pcfg.Settings{}
	if ownsAccount(tenantOwn, merged, keyField) {
		out[keyField] = tenantOwn[keyField]
		for _, k := range append(append([]string{}, identity...), presentation...) {
			out[k] = tenantOwn[k]
		}
		return out, true
	}
	out[keyField] = firstNonEmpty(platform[keyField], env[keyField])
	for _, k := range identity {
		out[k] = firstNonEmpty(platform[k], env[k])
	}
	for _, k := range presentation {
		out[k] = firstNonEmpty(tenantOwn[k], platform[k], env[k])
	}
	return out, false
}
