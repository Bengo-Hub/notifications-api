package messaging

import (
	"context"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// EmailSuppressedKeyPrefix is the Redis namespace of recipients no pod may send email to. The worker's
// email guard reads it before every send; the mail delivery webhook writes it when a message hard
// bounces, so both binaries must agree on one key format and live here.
const EmailSuppressedKeyPrefix = "notifications:email:suppressed:"

// EmailSuppressedKey is the Redis key for one address. Addresses are compared case-insensitively.
func EmailSuppressedKey(addr string) string {
	return EmailSuppressedKeyPrefix + strings.ToLower(strings.TrimSpace(addr))
}

// SuppressEmail marks an address as undeliverable for ttl. It reports whether the mark reached Redis;
// callers without Redis keep their own local copy.
func SuppressEmail(ctx context.Context, rdb redis.UniversalClient, addr string, ttl time.Duration) bool {
	if rdb == nil || strings.TrimSpace(addr) == "" {
		return false
	}
	return rdb.Set(ctx, EmailSuppressedKey(addr), "1", ttl).Err() == nil
}
