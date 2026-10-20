// Package platformkey holds the pieces shared by every platform-owner managed secret key: storage as
// a platform-level service config row (tenant_id IS NULL), random generation and the fingerprint the
// UI shows instead of key material. The provider-credential encryption key and the mail delivery
// webhook signing key are both managed through it, so there is one way to store, generate and rotate
// a platform key.
package platformkey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/bengobox/notifications-api/internal/ent"
	"github.com/bengobox/notifications-api/internal/ent/serviceconfig"
)

// Store reads and writes platform-level secret values in the service configs.
type Store struct{ client *ent.Client }

// NewStore wraps the ent client. A nil client yields a store that finds nothing.
func NewStore(client *ent.Client) *Store { return &Store{client: client} }

func (s *Store) row(ctx context.Context, key string) (*ent.ServiceConfig, error) {
	if s == nil || s.client == nil {
		return nil, nil
	}
	cfg, err := s.client.ServiceConfig.Query().
		Where(serviceconfig.ConfigKeyEQ(key), serviceconfig.TenantIDIsNil()).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return cfg, err
}

// Get returns a value and when it last changed. found is false when the key does not exist.
func (s *Store) Get(ctx context.Context, key string) (value string, updated time.Time, found bool, err error) {
	cfg, err := s.row(ctx, key)
	if err != nil || cfg == nil {
		return "", time.Time{}, false, err
	}
	return cfg.ConfigValue, cfg.UpdatedAt, true, nil
}

// Put creates or updates a platform-level secret. Config values may not be empty, so an empty value
// deletes the row. The row is always flagged secret so tenant views mask it.
func (s *Store) Put(ctx context.Context, key, value, description string) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("platformkey: no database client")
	}
	cfg, err := s.row(ctx, key)
	if err != nil {
		return err
	}
	switch {
	case value == "" && cfg != nil:
		return s.client.ServiceConfig.DeleteOne(cfg).Exec(ctx)
	case value == "":
		return nil
	case cfg != nil:
		return cfg.Update().SetConfigValue(value).SetConfigType("string").SetIsSecret(true).SetDescription(description).Exec(ctx)
	}
	return s.client.ServiceConfig.Create().
		SetConfigKey(key).
		SetConfigValue(value).
		SetConfigType("string").
		SetIsSecret(true).
		SetDescription(description).
		Exec(ctx) // tenant_id left nil: platform level
}

// Random returns n cryptographically random bytes.
func Random(n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("platformkey: generate key: %w", err)
	}
	return buf, nil
}

// Fingerprint is the first 8 hex characters of sha256(key): a non-reversible identifier that is safe
// to show, so an operator can tell two keys apart without ever seeing key material.
func Fingerprint(key []byte) string {
	if len(key) == 0 {
		return ""
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:8]
}
