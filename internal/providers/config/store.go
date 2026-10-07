package config

import (
	"context"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/database"
	"github.com/bengobox/notifications-api/internal/encryption"
	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/providersetting"
)

type Settings map[string]string

// LoadTenantProviderSettings loads provider settings for a tenant/channel/provider using Ent.
// Resolution hierarchy:
// 1. Platform-managed settings (is_platform_managed=true, tenant_id='platform')
// 2. Tenant-specific settings (tenant_id=tenantID)
// 3. Platform fallback settings (is_platform_managed=false, tenant_id='platform')
// If decryptionKey is non-nil (32 bytes), values stored with is_encrypted=true are decrypted.
func LoadTenantProviderSettings(ctx context.Context, dbCfg config.PostgresConfig, tenantID, environment, channel, provider string, decryptionKey []byte) (Settings, error) {
	return loadProviderSettings(ctx, dbCfg, tenantID, environment, channel, provider, decryptionKey, true)
}

// LoadTenantOnlyProviderSettings is LoadTenantProviderSettings with the platform-fallback and
// platform-managed tiers excluded — only rows saved directly under tenantID are considered. Unlike
// email (shared SMTP) and SMS (shared shortcode), a WhatsApp number is inherently tenant-specific:
// there is no sensible "shared platform WhatsApp number" a tenant without their own configured
// number should silently start sending through. Used by GetWhatsAppProvider so an unconfigured
// tenant gets a clear "not configured" error instead of unknowingly sending through whichever
// number the platform tier happens to hold (e.g. the platform's own operational WhatsApp account).
func LoadTenantOnlyProviderSettings(ctx context.Context, dbCfg config.PostgresConfig, tenantID, environment, channel, provider string, decryptionKey []byte) (Settings, error) {
	return loadProviderSettings(ctx, dbCfg, tenantID, environment, channel, provider, decryptionKey, false)
}

// settingsTTL is how long a resolved provider lookup is reused on this pod. A credential change
// takes effect within this window everywhere, which keeps bulk sends from querying the same rows
// for every message.
const settingsTTL = 30 * time.Second

var (
	clientsMu sync.Mutex
	clients   = map[string]*ent.Client{} // one pooled client per DSN for the process lifetime

	cacheMu sync.Mutex
	cache   = map[string]cachedSettings{}
)

type cachedSettings struct {
	settings Settings
	expires  time.Time
}

// sharedClient opens the ent client for dsn once and reuses it. Opening (and closing) a client
// per call, three times per send, was the main cost of a send under load.
func sharedClient(ctx context.Context, dsn string) (*ent.Client, error) {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	if c, ok := clients[dsn]; ok {
		return c, nil
	}
	c, err := database.NewClient(ctx, config.PostgresConfig{URL: dsn})
	if err != nil {
		return nil, err
	}
	clients[dsn] = c
	return c, nil
}

// InvalidateCache drops every cached lookup on this pod (call after saving provider settings).
func InvalidateCache() {
	cacheMu.Lock()
	cache = map[string]cachedSettings{}
	cacheMu.Unlock()
}

func loadProviderSettings(ctx context.Context, dbCfg config.PostgresConfig, tenantID, environment, channel, provider string, decryptionKey []byte, includePlatformTiers bool) (Settings, error) {
	dsn := dbCfg.URL
	if env := os.Getenv("POSTGRES_URL"); env != "" {
		dsn = env
	} else if env := os.Getenv("NOTIFICATIONS_POSTGRES_URL"); env != "" {
		dsn = env
	}
	if dsn == "" {
		return Settings{}, nil
	}

	key := strings.Join([]string{tenantID, environment, channel, provider, strconv.FormatBool(includePlatformTiers)}, "|")
	cacheMu.Lock()
	if hit, ok := cache[key]; ok && time.Now().Before(hit.expires) {
		cacheMu.Unlock()
		return maps.Clone(hit.settings), nil
	}
	cacheMu.Unlock()

	client, err := sharedClient(ctx, dsn)
	if err != nil {
		return Settings{}, err
	}
	out, err := querySettings(ctx, client, tenantID, environment, channel, provider, decryptionKey, includePlatformTiers)
	if err != nil {
		return Settings{}, err
	}
	cacheMu.Lock()
	cache[key] = cachedSettings{settings: maps.Clone(out), expires: time.Now().Add(settingsTTL)}
	cacheMu.Unlock()
	return out, nil
}

func querySettings(ctx context.Context, client *ent.Client, tenantID, environment, channel, provider string, decryptionKey []byte, includePlatformTiers bool) (Settings, error) {

	// Query tenant settings, plus platform settings too unless the caller explicitly wants only
	// what's saved directly under tenantID (see LoadTenantOnlyProviderSettings).
	tenantIDs := []string{tenantID}
	if includePlatformTiers {
		tenantIDs = append(tenantIDs, "platform")
	}
	rows, err := client.ProviderSetting.
		Query().
		Where(
			providersetting.TenantIDIn(tenantIDs...),
			providersetting.Or(
				providersetting.EnvironmentEQ(environment),
				providersetting.EnvironmentEQ("production"),
			),
			providersetting.ChannelEQ(channel),
			providersetting.ProviderEQ(provider),
			providersetting.IsActive(true),
		).
		All(ctx)
	if err != nil {
		return Settings{}, err
	}

	platformManaged := Settings{}
	tenantSpecific := Settings{}
	platformFallback := Settings{}

	for _, r := range rows {
		val := r.Value
		if r.IsEncrypted && len(decryptionKey) == 32 {
			if dec, err := encryption.Decrypt(val, decryptionKey); err == nil {
				val = dec
			}
		}

		if r.TenantID == "platform" {
			if r.IsPlatformManaged {
				platformManaged[r.Key] = val
			} else {
				platformFallback[r.Key] = val
			}
		} else {
			tenantSpecific[r.Key] = val
		}
	}

	// Merge with hierarchy: platformFallback < tenantSpecific < platformManaged
	out := platformFallback
	for k, v := range tenantSpecific {
		out[k] = v
	}
	for k, v := range platformManaged {
		out[k] = v
	}

	return out, nil
}
