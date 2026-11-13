package wacost

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisCache stores reports in Redis. A Redis failure is a cache miss, never a failed report.
type RedisCache struct {
	rdb redis.UniversalClient
}

// NewRedisCache returns nil when rdb is nil, so the service runs uncached.
func NewRedisCache(rdb redis.UniversalClient) Cache {
	if rdb == nil {
		return nil
	}
	return &RedisCache{rdb: rdb}
}

// Get reads a cached report.
func (c *RedisCache) Get(ctx context.Context, key string) ([]byte, bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	b, err := c.rdb.Get(cctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	return b, true
}

// Set stores a report for ttl.
func (c *RedisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = c.rdb.Set(cctx, key, value, ttl).Err()
}
