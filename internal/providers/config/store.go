package config

import (
	"context"
	"os"

	"github.com/bengobox/notifications-api/internal/config"
	"github.com/bengobox/notifications-api/internal/database"
	"github.com/bengobox/notifications-api/internal/encryption"
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
	dsn := dbCfg.URL
	if env := os.Getenv("POSTGRES_URL"); env != "" {
		dsn = env
	} else if env := os.Getenv("NOTIFICATIONS_POSTGRES_URL"); env != "" {
		dsn = env
	}
	if dsn == "" {
		return Settings{}, nil
	}
	client, err := database.NewClient(ctx, config.PostgresConfig{URL: dsn})
	if err != nil {
		return Settings{}, err
	}
	defer client.Close()

	// Query both tenant and platform settings in one go
	rows, err := client.ProviderSetting.
		Query().
		Where(
			providersetting.TenantIDIn(tenantID, "platform"),
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
