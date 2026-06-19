package encryption

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sync"
	"time"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/serviceconfig"
)

// DBConfigKey is the ServiceConfig.config_key under which the platform-owner-managed
// AES-256-GCM provider-credential encryption key is stored (base64-std of 32 bytes,
// tenant_id IS NULL).
const DBConfigKey = "encryption_key"

// keyCacheTTL bounds how long resolved candidate keys are cached before the DB is
// re-read. Short enough that a platform-owner key rotation takes effect quickly.
const keyCacheTTL = 30 * time.Second

// KeyProvider resolves the ordered list of candidate AES-256 keys used to encrypt and
// decrypt provider credentials at rest. Resolution is DB-first with env fallback:
//
//	(a) DB key  — ServiceConfig where config_key=="encryption_key" AND tenant_id IS NULL
//	              (config_value is base64-std of exactly 32 bytes) => PRIMARY.
//	(b) ENV key — KeyFromEnv(cfg.Security.EncryptionKey).
//
// There is no dev fallback. If neither is present the candidate list is empty and
// encryption is disabled (matching the prior bare-key nil behavior). Results are cached
// for ~30s; call Invalidate after writing a new DB key.
type KeyProvider struct {
	client *ent.Client
	envKey []byte // pre-validated 32-byte env key (or nil)

	mu        sync.RWMutex
	cached    [][]byte
	cachedAt  time.Time
	cachedSrc string // "db" or "env" for the PRIMARY key, "" if none
}

// NewKeyProvider builds a KeyProvider. envKey is the raw value from config
// (cfg.Security.EncryptionKey); it is parsed via KeyFromEnv so only a valid 32-byte
// key is retained. client may be nil, in which case only the env key is ever used.
func NewKeyProvider(client *ent.Client, envKeyRaw string) *KeyProvider {
	return &KeyProvider{
		client: client,
		envKey: KeyFromEnv(envKeyRaw),
	}
}

// Invalidate clears the cached candidate keys so the next resolution re-reads the DB.
func (p *KeyProvider) Invalidate() {
	p.mu.Lock()
	p.cached = nil
	p.cachedAt = time.Time{}
	p.cachedSrc = ""
	p.mu.Unlock()
}

// dbKey reads the platform-level (tenant_id IS NULL) encryption_key from ServiceConfig.
// Returns nil if absent, unreadable, or not exactly 32 bytes after base64 decode.
func (p *KeyProvider) dbKey(ctx context.Context) []byte {
	if p.client == nil {
		return nil
	}
	row, err := p.client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKeyEQ(DBConfigKey),
			serviceconfig.TenantIDIsNil(),
		).
		First(ctx)
	if err != nil || row == nil {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(row.ConfigValue)
	if err != nil || len(key) != 32 {
		return nil
	}
	return key
}

// resolve returns the ordered, de-duplicated candidate keys plus the source of the
// PRIMARY (candidates[0]) key. Uses the cache when fresh.
func (p *KeyProvider) resolve(ctx context.Context) ([][]byte, string) {
	p.mu.RLock()
	if p.cached != nil && time.Since(p.cachedAt) < keyCacheTTL {
		keys, src := p.cached, p.cachedSrc
		p.mu.RUnlock()
		return keys, src
	}
	p.mu.RUnlock()

	// Build candidate list: DB key first (PRIMARY), then env key. Dedupe.
	var candidates [][]byte
	src := ""

	if dk := p.dbKey(ctx); len(dk) == 32 {
		candidates = append(candidates, dk)
		src = "db"
	}
	if len(p.envKey) == 32 {
		if !containsKey(candidates, p.envKey) {
			if src == "" {
				src = "env"
			}
			candidates = append(candidates, p.envKey)
		}
	}

	p.mu.Lock()
	p.cached = candidates
	p.cachedAt = time.Now()
	p.cachedSrc = src
	p.mu.Unlock()

	return candidates, src
}

// Candidates returns the ordered candidate keys (PRIMARY first). May be empty.
func (p *KeyProvider) Candidates(ctx context.Context) [][]byte {
	keys, _ := p.resolve(ctx)
	return keys
}

// Primary returns the PRIMARY (highest-priority) 32-byte key, or nil if none.
func (p *KeyProvider) Primary(ctx context.Context) []byte {
	keys, _ := p.resolve(ctx)
	if len(keys) == 0 {
		return nil
	}
	return keys[0]
}

// Source returns "db", "env", or "" indicating where the PRIMARY key came from.
func (p *KeyProvider) Source(ctx context.Context) string {
	_, src := p.resolve(ctx)
	return src
}

// Configured reports whether any usable key is available.
func (p *KeyProvider) Configured(ctx context.Context) bool {
	keys, _ := p.resolve(ctx)
	return len(keys) > 0
}

// Fingerprint returns the first 8 hex chars of sha256(PRIMARY key), or "" if no key.
// This is a non-reversible identifier safe to expose; it never reveals key material.
func (p *KeyProvider) Fingerprint(ctx context.Context) string {
	key := p.Primary(ctx)
	if len(key) == 0 {
		return ""
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:8]
}

// Encrypt encrypts plaintext with the PRIMARY key. If no key is configured it returns
// the plaintext unchanged with ok=false, matching the prior no-key behavior where
// secrets were stored unencrypted.
func (p *KeyProvider) Encrypt(ctx context.Context, plaintext string) (value string, ok bool, err error) {
	key := p.Primary(ctx)
	if len(key) != 32 {
		return plaintext, false, nil
	}
	enc, err := Encrypt(plaintext, key)
	if err != nil {
		return plaintext, false, err
	}
	return enc, true, nil
}

// Decrypt tries each candidate key (AES-256-GCM); the first that succeeds wins.
//
// Backward compatibility: if every key fails AND the encoded value does not look like
// valid ciphertext (i.e. it appears to be plaintext that was stored before encryption
// was enabled), the value is returned unchanged so previously-unencrypted credentials
// remain readable. If it does look like ciphertext but no key opens it (e.g. wrong key),
// the original value is returned so callers degrade gracefully rather than corrupting.
func (p *KeyProvider) Decrypt(ctx context.Context, encoded string) string {
	if encoded == "" {
		return ""
	}
	for _, key := range p.Candidates(ctx) {
		if dec, err := Decrypt(encoded, key); err == nil {
			return dec
		}
	}
	// No key opened it. Return as-is (covers plaintext-at-rest legacy values; also a
	// safe degrade for ciphertext we lack a key for — never returns garbage).
	return encoded
}

// containsKey reports whether k is already present in keys.
func containsKey(keys [][]byte, k []byte) bool {
	for _, existing := range keys {
		if bytes.Equal(existing, k) {
			return true
		}
	}
	return false
}
