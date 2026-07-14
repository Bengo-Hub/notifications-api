package main

import (
	"context"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// emailGuard protects the shared email provider account on two fronts:
//   1. Rate-limit blocks — paces sends with a token bucket so a backlog drain or order
//      burst never trips the provider's hourly limit (e.g. Zoho "mail rate exceeded").
//   2. Reputation / noisy failure logs — validates recipients (RFC syntax + MX
//      resolvability + disposable/placeholder blocklist) and suppresses addresses that
//      hard-bounced (550 5.1.x), so we stop sending to invalid mailboxes. (Per email
//      deliverability best practice: keep hard-bounce rate <2%; "no MX" = hard bounce.)
//
// All state is in-process and concurrency-safe. Suppression is best-effort within a pod
// lifetime — a Redis/DB-backed suppression list fed by bounce webhooks is the production
// upgrade (tracked as a follow-up).
type emailGuard struct {
	log        *zap.Logger
	validateMX bool

	// token-bucket rate limiter
	mu        sync.Mutex
	tokens    float64
	maxTokens float64
	perSec    float64
	last      time.Time

	// MX-resolvability cache (domain -> result, TTL'd) to avoid a DNS lookup per send
	mxMu    sync.Mutex
	mxCache map[string]mxEntry

	// in-process suppression of hard-bounced / invalid recipients
	supMu      sync.Mutex
	suppressed map[string]time.Time

	// circuit breaker for tenant providers with failing credentials: a provider that
	// fails auth is skipped (straight to platform fallback) until the cooldown expires,
	// instead of hammering the SMTP host with a doomed AUTH per message — bursts of
	// failed logins are exactly what trips Zoho's abuse detection.
	provMu       sync.Mutex
	provCooldown map[string]time.Time
}

type mxEntry struct {
	ok  bool
	exp time.Time
}

// disposable/placeholder domains we never attempt to send to (guaranteed bounces / noise).
var blockedEmailDomains = map[string]bool{
	"example.com": true, "example.org": true, "example.net": true, "test.com": true,
	"test.test": true, "email.com": true, "domain.com": true, "localhost": true,
	"mailinator.com": true, "yopmail.com": true, "guerrillamail.com": true,
	"10minutemail.com": true, "tempmail.com": true, "trashmail.com": true, "sharklasers.com": true,
}

func newEmailGuard(maxPerHour, burst int, validateMX bool, log *zap.Logger) *emailGuard {
	if maxPerHour <= 0 {
		maxPerHour = 200
	}
	if burst <= 0 {
		burst = 25
	}
	return &emailGuard{
		log:        log.Named("email-guard"),
		validateMX: validateMX,
		tokens:     float64(burst),
		maxTokens:  float64(burst),
		perSec:     float64(maxPerHour) / 3600.0,
		last:       time.Now(),
		mxCache:      make(map[string]mxEntry),
		suppressed:   make(map[string]time.Time),
		provCooldown: make(map[string]time.Time),
	}
}

// ProviderCoolingDown reports whether the given provider key (tenant id) recently failed
// authentication and should be skipped in favor of the platform provider.
func (g *emailGuard) ProviderCoolingDown(key string) bool {
	g.provMu.Lock()
	defer g.provMu.Unlock()
	exp, ok := g.provCooldown[key]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(g.provCooldown, key)
		return false
	}
	return true
}

// CoolProvider opens the circuit for a provider key after an auth failure.
func (g *emailGuard) CoolProvider(key string, ttl time.Duration) {
	if key == "" {
		return
	}
	g.provMu.Lock()
	g.provCooldown[key] = time.Now().Add(ttl)
	g.provMu.Unlock()
	g.log.Warn("tenant email provider cooling down after auth failure",
		zap.String("tenant_id", key), zap.Duration("ttl", ttl))
}

// isAuthFailureError reports whether an SMTP error means the provider's own credentials
// are bad (535 / authentication failed) — a config problem that will fail identically on
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
// Skipping bad recipients up front avoids guaranteed bounces (which hurt reputation and
// flood the logs) without ever attempting the send.
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
// implicit MX fallback). Results are cached for 6h (negative for 30m) to bound DNS load.
func (g *emailGuard) domainResolvable(domain string) bool {
	g.mxMu.Lock()
	if e, ok := g.mxCache[domain]; ok && time.Now().Before(e.exp) {
		g.mxMu.Unlock()
		return e.ok
	}
	g.mxMu.Unlock()

	ok := lookupMailHost(domain)
	ttl := 6 * time.Hour
	if !ok {
		ttl = 30 * time.Minute // re-check sooner in case of transient DNS failure
	}
	g.mxMu.Lock()
	g.mxCache[domain] = mxEntry{ok: ok, exp: time.Now().Add(ttl)}
	g.mxMu.Unlock()
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
	g.supMu.Lock()
	defer g.supMu.Unlock()
	exp, ok := g.suppressed[key]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(g.suppressed, key)
		return false
	}
	return true
}

// Suppress records a hard-bounced / invalid recipient so we stop sending to it for ttl.
func (g *emailGuard) Suppress(addr string, ttl time.Duration) {
	if addr == "" {
		return
	}
	g.supMu.Lock()
	g.suppressed[strings.ToLower(addr)] = time.Now().Add(ttl)
	g.supMu.Unlock()
	g.log.Warn("recipient suppressed after hard bounce", zap.String("to", addr), zap.Duration("ttl", ttl))
}

// WaitForSlot paces sends: it blocks until a token is available or maxWait elapses
// (maxWait is bounded well under the broker AckWait so a paced message is never
// redelivered). Returns whether a token was actually granted.
func (g *emailGuard) WaitForSlot(ctx context.Context, maxWait time.Duration) bool {
	deadline := time.Now().Add(maxWait)
	for {
		if g.take() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-time.After(400 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
	}
}

func (g *emailGuard) take() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.tokens += now.Sub(g.last).Seconds() * g.perSec
	if g.tokens > g.maxTokens {
		g.tokens = g.maxTokens
	}
	g.last = now
	if g.tokens >= 1 {
		g.tokens--
		return true
	}
	return false
}

// isPermanentRecipientError reports whether an SMTP error means the mailbox is bad
// (550 5.1.x — no such user / does not exist), which should suppress the recipient.
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
