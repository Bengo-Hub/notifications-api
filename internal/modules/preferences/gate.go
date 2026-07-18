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

// Enabled reports whether templateID may be delivered for the tenant. tenantID should
// be the resolved tenant UUID string; an empty/unparseable tenant skips the tenant
// override layer (platform default + registry still apply).
func (g *Gate) Enabled(ctx context.Context, tenantID, templateID string) bool {
	if templateID == "" {
		return true
	}
	if IsLocked(templateID) {
		return true
	}

	cacheKey := "notifprefs:" + tenantID + ":" + templateID
	if g.cache != nil {
		if v, err := g.cache.Get(ctx, cacheKey).Result(); err == nil {
			return v == "1"
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
	return enabled
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
