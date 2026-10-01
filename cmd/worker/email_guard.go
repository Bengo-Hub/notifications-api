package main

import (
	"context"
	"net"
	"net/mail"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// emailGuard protects the shared email provider account on two fronts:
//  1. Rate-limit blocks: paces sends so a backlog drain or order burst never trips the
//     provider's hourly limit (e.g. "mail rate exceeded").
//  2. Reputation and noisy failure logs: validates recipients (RFC syntax, MX resolvability,
//     disposable/placeholder blocklist) and suppresses addresses that hard-bounced (550 5.1.x).
//
// Every worker pod sends through the same provider account, so the pacing budget, the
// suppression list and provider cooldowns are shared in Redis: before, each pod kept its own
// copy in memory, so N pods sent N times the provider limit and a bounce seen by one pod was
// retried by the others. Without Redis the limiter falls back to a per-pod share and the lists
// to bounded per-pod caches. The MX cache stays per pod (it only saves DNS lookups).
type emailGuard struct {
	log        *zap.Logger
	validateMX bool
	rdb        redis.UniversalClient

	limiter *ratelimit.Limiter
	pace    ratelimit.Options

	mxOK  *sharedcache.Local[string, bool] // positive results, 6h
	mxBad *sharedcache.Local[string, bool] // negative results, 30m (re-check sooner)

	localSuppressed *sharedcache.Local[string, bool] // used only when Redis is unavailable
	localCooldown   *sharedcache.Local[string, bool]
}

// disposable/placeholder domains we never attempt to send to (guaranteed bounces / noise).
var blockedEmailDomains = map[string]bool{
	"example.com": true, "example.org": true, "example.net": true, "test.com": true,
	"test.test": true, "email.com": true, "domain.com": true, "localhost": true,
	"mailinator.com": true, "yopmail.com": true, "guerrillamail.com": true,
	"10minutemail.com": true, "tempmail.com": true, "trashmail.com": true, "sharklasers.com": true,
}

const (
	suppressedKeyPrefix = "notifications:email:suppressed:"
	cooldownKeyPrefix   = "notifications:email:provider-cooldown:"
)

func newEmailGuard(rdb redis.UniversalClient, maxPerHour, burst int, validateMX bool, log *zap.Logger) *emailGuard {
	if maxPerHour <= 0 {
		maxPerHour = 200
	}
	if burst <= 0 {
		burst = 25
	}
	if c, ok := rdb.(*redis.Client); ok && c == nil {
		rdb = nil // a typed nil client must not look like a live one
	}
	l := log.Named("email-guard")
	return &emailGuard{
		log:        l,
		validateMX: validateMX,
		rdb:        rdb,
		limiter:    ratelimit.NewLimiter(rdb, l, "notifications"),
		pace: ratelimit.Options{
			Name: "email-provider", Limit: maxPerHour, Window: time.Hour, Burst: burst,
		},
		mxOK:            sharedcache.NewLocal[string, bool](10000, 6*time.Hour),
		mxBad:           sharedcache.NewLocal[string, bool](10000, 30*time.Minute),
		localSuppressed: sharedcache.NewLocal[string, bool](50000, 30*24*time.Hour),
		localCooldown:   sharedcache.NewLocal[string, bool](5000, time.Hour),
	}
}

func (g *emailGuard) redisOK() bool { return g.rdb != nil }

// ProviderCoolingDown reports whether the given provider key (tenant id) recently failed
// authentication and should be skipped in favor of the platform provider.
func (g *emailGuard) ProviderCoolingDown(key string) bool {
	if g.redisOK() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		n, err := g.rdb.Exists(ctx, cooldownKeyPrefix+key).Result()
		if err == nil {
			return n > 0
		}
	}
	_, ok := g.localCooldown.Get(key)
	return ok
}

// CoolProvider opens the circuit for a provider key after an auth failure, on every pod.
func (g *emailGuard) CoolProvider(key string, ttl time.Duration) {
	if key == "" {
		return
	}
	if g.redisOK() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		err := g.rdb.Set(ctx, cooldownKeyPrefix+key, "1", ttl).Err()
		cancel()
		if err != nil {
			g.localCooldown.Set(key, true)
		}
	} else {
		g.localCooldown.Set(key, true)
	}
	g.log.Warn("tenant email provider cooling down after auth failure",
		zap.String("tenant_id", key), zap.Duration("ttl", ttl))
}

// isAuthFailureError reports whether an SMTP error means the provider's own credentials
// are bad (535 / authentication failed): a config problem that will fail identically on
// every send until the tenant fixes their provider settings.
func isAuthFailureError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "535") || strings.Contains(s, "authentication failed") ||
		strings.Contains(s, "auth: ")
}

// ValidRecipients returns the subset of addrs safe to send to, plus the skipped ones.
func (g *emailGuard) ValidRecipients(addrs []string) (valid, skipped []string) {
	for _, a := range addrs {
		if reason := g.reject(a); reason != "" {
			skipped = append(skipped, a)
			g.log.Warn("skipping email recipient", zap.String("to", a), zap.String("reason", reason))
			continue
		}
		valid = append(valid, a)
	}
	return valid, skipped
}

func (g *emailGuard) reject(addr string) string {
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return "invalid syntax"
	}
	at := strings.LastIndex(parsed.Address, "@")
	if at <= 0 || at == len(parsed.Address)-1 {
		return "invalid syntax"
	}
	domain := strings.ToLower(parsed.Address[at+1:])
	if blockedEmailDomains[domain] {
		return "disposable/placeholder domain"
	}
	if g.isSuppressed(parsed.Address) {
		return "suppressed (prior hard bounce)"
	}
	if g.validateMX && !g.domainResolvable(domain) {
		return "unresolvable domain (no MX/A record)"
	}
	return ""
}

// domainResolvable reports whether the domain can receive mail (has MX, or A/AAAA as an
// implicit MX fallback). Cached per pod: 6h positive, 30m negative, bounded entries.
func (g *emailGuard) domainResolvable(domain string) bool {
	if _, ok := g.mxOK.Get(domain); ok {
		return true
	}
	if _, ok := g.mxBad.Get(domain); ok {
		return false
	}
	ok := lookupMailHost(domain)
	if ok {
		g.mxOK.Set(domain, true)
	} else {
		g.mxBad.Set(domain, true)
	}
	return ok
}

func lookupMailHost(domain string) bool {
	if mxs, err := net.LookupMX(domain); err == nil && len(mxs) > 0 {
		return true
	}
	// RFC 5321: a domain with no MX but an A/AAAA record still accepts mail (implicit MX).
	if ips, err := net.LookupHost(domain); err == nil && len(ips) > 0 {
		return true
	}
	return false
}

func (g *emailGuard) isSuppressed(addr string) bool {
	key := strings.ToLower(addr)
	if g.redisOK() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		n, err := g.rdb.Exists(ctx, suppressedKeyPrefix+key).Result()
		if err == nil {
			return n > 0
		}
	}
	_, ok := g.localSuppressed.Get(key)
	return ok
}

// Suppress records a hard-bounced / invalid recipient so no pod sends to it for ttl.
func (g *emailGuard) Suppress(addr string, ttl time.Duration) {
	if addr == "" {
		return
	}
	key := strings.ToLower(addr)
	stored := false
	if g.redisOK() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		stored = g.rdb.Set(ctx, suppressedKeyPrefix+key, "1", ttl).Err() == nil
		cancel()
	}
	if !stored {
		g.localSuppressed.Set(key, true)
	}
	g.log.Warn("recipient suppressed after hard bounce", zap.String("to", addr), zap.Duration("ttl", ttl))
}

// WaitForSlot paces sends: it blocks until the fleet-wide budget has room or maxWait elapses
// (maxWait is bounded well under the broker AckWait so a paced message is never
// redelivered). Returns whether a slot was granted.
func (g *emailGuard) WaitForSlot(ctx context.Context, maxWait time.Duration) bool {
	deadline := time.Now().Add(maxWait)
	for {
		ok, retryAfter := g.limiter.Allow(ctx, "send", g.pace, 1)
		if ok {
			return true
		}
		wait := retryAfter
		if wait <= 0 || wait > time.Until(deadline) {
			wait = time.Until(deadline)
		}
		if wait <= 0 {
			return false
		}
		if wait > 2*time.Second {
			wait = 2 * time.Second // re-check: another pod's slot may free up sooner
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false
		}
	}
}

// isPermanentRecipientError reports whether an SMTP error means the mailbox is bad
// (550 5.1.x: no such user / does not exist), which should suppress the recipient.
// A 550 5.4.6 (rate/policy) is NOT a recipient problem and must not suppress.
func isPermanentRecipientError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "5.4.6") || strings.Contains(s, "rate") || strings.Contains(s, "unusual sending") {
		return false
	}
	return strings.Contains(s, "5.1.1") || strings.Contains(s, "5.1.0") ||
		strings.Contains(s, "550 5.1") || strings.Contains(s, "no such user") ||
		strings.Contains(s, "does not exist") || strings.Contains(s, "user unknown") ||
		strings.Contains(s, "recipient address rejected") || strings.Contains(s, "mailbox unavailable")
}
