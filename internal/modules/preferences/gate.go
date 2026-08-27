package preferences

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/serviceconfig"
)

// cacheTTL bounds how stale a toggle read may be — the gate runs on every send, so
// lookups are cached briefly in Redis (a settings change propagates within this TTL).
const cacheTTL = 60 * time.Second

// Gate answers "may this notification type be delivered to this tenant?" for the
// worker's dispatch path. Resolution order:
//
//	locked registry class → tenant ServiceConfig override → platform ServiceConfig
//	default → registry class default (essential=on, optional=off, unknown=on).
//
// Fail-open on storage errors: a broken config store must never black-hole
// security or transactional notifications.
type Gate struct {
	client *ent.Client
	cache  *redis.Client
	log    *zap.Logger
}

// NewGate builds the gate. cache may be nil (no caching, direct DB reads).
func NewGate(client *ent.Client, cache *redis.Client, log *zap.Logger) *Gate {
	return &Gate{client: client, cache: cache, log: log.Named("notification-gate")}
}

// Enabled reports whether templateID may be delivered for the tenant on the given channel.
// tenantID should be the resolved tenant UUID string; an empty/unparseable tenant skips the
// tenant override layer (platform default + registry still apply). channel is optional — pass
// "" to check only the type-level toggle (e.g. the settings UI, which shows channel selection
// separately); the worker's real dispatch path always passes the message's actual channel so a
// tenant that unchecked, say, SMS for a type that also sends email doesn't get gated entirely.
func (g *Gate) Enabled(ctx context.Context, tenantID, templateID, channel string) bool {
	if templateID == "" {
		return true
	}
	if IsLocked(templateID) {
		return true
	}

	cacheKey := "notifprefs:" + tenantID + ":" + templateID
	if g.cache != nil {
		if v, err := g.cache.Get(ctx, cacheKey).Result(); err == nil {
			if v != "1" {
				return false
			}
			return g.channelEnabled(ctx, tenantID, templateID, channel)
		}
	}

	enabled := g.resolve(ctx, tenantID, templateID)

	if g.cache != nil {
		v := "0"
		if enabled {
			v = "1"
		}
		_ = g.cache.Set(ctx, cacheKey, v, cacheTTL).Err()
	}
	if !enabled {
		return false
	}
	return g.channelEnabled(ctx, tenantID, templateID, channel)
}

// channelEnabled checks the tenant's chosen channel SUBSET for templateID (see
// ChannelsConfigKey) — absence of any override row means every channel is enabled, the
// pre-existing behavior before per-channel selection existed. Fails open (true) on any
// storage error or when channel is "" (caller only cares about the type-level toggle).
func (g *Gate) channelEnabled(ctx context.Context, tenantID, templateID, channel string) bool {
	if channel == "" || g.client == nil {
		return true
	}
	key := ChannelsConfigKey(templateID)

	if tid, err := uuid.Parse(tenantID); err == nil && tid != uuid.Nil {
		row, err := g.client.ServiceConfig.Query().
			Where(serviceconfig.ConfigKeyEQ(key), serviceconfig.TenantIDEQ(tid)).
			First(ctx)
		if err == nil {
			return channelInList(row.ConfigValue, channel)
		}
		if !ent.IsNotFound(err) {
			return true // fail open
		}
	}

	row, err := g.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKeyEQ(key), serviceconfig.TenantIDIsNil()).
		First(ctx)
	if err == nil {
		return channelInList(row.ConfigValue, channel)
	}
	return true // no override at any level — every channel enabled
}

func channelInList(csv, channel string) bool {
	for _, c := range strings.Split(csv, ",") {
		if strings.EqualFold(strings.TrimSpace(c), channel) {
			return true
		}
	}
	return false
}

func (g *Gate) resolve(ctx context.Context, tenantID, templateID string) bool {
	if g.client == nil {
		return DefaultEnabled(templateID)
	}
	key := ConfigKey(templateID)

	// Tenant override wins.
	if tid, err := uuid.Parse(tenantID); err == nil && tid != uuid.Nil {
		row, err := g.client.ServiceConfig.Query().
			Where(serviceconfig.ConfigKeyEQ(key), serviceconfig.TenantIDEQ(tid)).
			First(ctx)
		if err == nil {
			return parseBool(row.ConfigValue, DefaultEnabled(templateID))
		}
		if !ent.IsNotFound(err) {
			g.log.Warn("notification gate: tenant config read failed (fail-open to default)",
				zap.String("tenant_id", tenantID), zap.String("key", key), zap.Error(err))
			return DefaultEnabled(templateID)
		}
	}

	// Platform-level default row (tenant_id IS NULL).
	row, err := g.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKeyEQ(key), serviceconfig.TenantIDIsNil()).
		First(ctx)
	if err == nil {
		return parseBool(row.ConfigValue, DefaultEnabled(templateID))
	}
	if !ent.IsNotFound(err) {
		g.log.Warn("notification gate: platform config read failed (fail-open to default)",
			zap.String("key", key), zap.Error(err))
	}

	return DefaultEnabled(templateID)
}

// Invalidate drops the cached toggle for (tenant, template) after a settings change.
func (g *Gate) Invalidate(ctx context.Context, tenantID, templateID string) {
	if g.cache == nil {
		return
	}
	_ = g.cache.Del(ctx, "notifprefs:"+tenantID+":"+templateID).Err()
}

func parseBool(raw string, fallback bool) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return v
}
