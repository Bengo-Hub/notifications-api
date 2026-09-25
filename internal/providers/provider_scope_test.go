package providers

import (
	"testing"

	pcfg "github.com/bengobox/notifications-api/internal/providers/config"
)

var platformSMTP = pcfg.Settings{"host": "smtp.platform.example", "port": "465", "username": "ops@platform.example", "password": "platform-secret", "from": "no-reply@platform.example"}

func merge(platform, tenant pcfg.Settings) pcfg.Settings {
	out := pcfg.Settings{}
	for k, v := range platform {
		out[k] = v
	}
	for k, v := range tenant {
		out[k] = v
	}
	return out
}

func TestResolveSMTP_TenantHostNeverGetsPlatformPassword(t *testing.T) {
	tenant := pcfg.Settings{"host": "mail.tenant.example"}
	acct := resolveSMTP(tenant, platformSMTP, merge(platformSMTP, tenant), smtpAccount{}, false)
	if !acct.Own || acct.Host != "mail.tenant.example" {
		t.Fatalf("tenant host should select the tenant account: %#v", acct)
	}
	if acct.Password != "" || acct.Username != "" {
		t.Fatalf("platform credentials leaked to the tenant's server: %#v", acct)
	}
}

func TestResolveSMTP_PlatformAccountWithTenantFrom(t *testing.T) {
	tenant := pcfg.Settings{"from": "orders@tenant.example"}
	acct := resolveSMTP(tenant, platformSMTP, merge(platformSMTP, tenant), smtpAccount{}, false)
	if acct.Own || acct.Host != "smtp.platform.example" || acct.Password != "platform-secret" {
		t.Fatalf("tenant without a host must use the platform account: %#v", acct)
	}
	if acct.From != "orders@tenant.example" {
		t.Fatalf("tenant sender address should still apply: %#v", acct)
	}
}

func TestResolveSMTP_LocalhostTenantIgnoredOutsideDev(t *testing.T) {
	tenant := pcfg.Settings{"host": "localhost", "password": "x"}
	if acct := resolveSMTP(tenant, platformSMTP, merge(platformSMTP, tenant), smtpAccount{}, false); acct.Own {
		t.Fatalf("localhost tenant host must fall back to the platform in production")
	}
	if acct := resolveSMTP(tenant, platformSMTP, merge(platformSMTP, tenant), smtpAccount{}, true); !acct.Own {
		t.Fatalf("localhost tenant host is fine in development")
	}
}

func TestResolveSMTP_PlatformManagedHostWins(t *testing.T) {
	// Platform-managed host forces the platform server even if the tenant saved its own.
	tenant := pcfg.Settings{"host": "mail.tenant.example", "password": "t"}
	merged := merge(tenant, platformSMTP)
	if acct := resolveSMTP(tenant, platformSMTP, merged, smtpAccount{}, false); acct.Own {
		t.Fatalf("a platform-managed host must not be overridden")
	}
}

func TestResolveKeyedAccount(t *testing.T) {
	platform := pcfg.Settings{"api_key": "platform-key", "username": "platform", "from": "CODEVERTEX"}
	env := map[string]string{"api_key": "env-key", "username": "env"}

	// Tenant only chose a sender ID: platform account, tenant sender ID.
	tenant := pcfg.Settings{"from": "URBANLOFT", "username": "tenant-user"}
	s, own := resolveKeyedAccount(tenant, platform, merge(platform, tenant), "api_key", []string{"username"}, []string{"from"}, env)
	if own || s["api_key"] != "platform-key" || s["username"] != "platform" || s["from"] != "URBANLOFT" {
		t.Fatalf("username must follow the platform key; sender ID may be the tenant's: %#v", s)
	}

	// Tenant brought its own key: its own username too, never the platform's.
	tenant = pcfg.Settings{"api_key": "tenant-key"}
	s, own = resolveKeyedAccount(tenant, platform, merge(platform, tenant), "api_key", []string{"username"}, []string{"from"}, env)
	if !own || s["api_key"] != "tenant-key" || s["username"] != "" {
		t.Fatalf("tenant key must not be paired with the platform username: %#v", s)
	}

	// Nothing in DB: env.
	s, _ = resolveKeyedAccount(pcfg.Settings{}, pcfg.Settings{}, pcfg.Settings{}, "api_key", []string{"username"}, nil, env)
	if s["api_key"] != "env-key" || s["username"] != "env" {
		t.Fatalf("env fallback missing: %#v", s)
	}
}
