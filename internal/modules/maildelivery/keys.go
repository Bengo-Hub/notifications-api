package maildelivery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bengobox/notifications-api/internal/platformkey"
)

// Platform config keys holding the webhook signing key. The previous key stays valid after a
// rotation so batches signed just before the switch are still accepted.
const (
	KeyConfigKey         = "mail.delivery_webhook_key"
	PreviousKeyConfigKey = "mail.delivery_webhook_key_previous"
	keyCacheTTL          = 15 * time.Second

	keyDescription         = "Shared signing key of the mail server's delivery webhook. Managed from the Platform page, rotate it there rather than editing it here."
	previousKeyDescription = "Previous delivery webhook signing key, still accepted for a short time after a rotation."
)

// Applier pushes a new signing key to the mail server. It is called after the key is stored, so the
// receiver already accepts it when the first signed batch arrives.
type Applier func(ctx context.Context, key string) error

// Keys reads and rotates the signing keys, which live in the platform service configs so an
// operator can rotate them from the notifications UI without redeploying anything.
type Keys struct {
	cfg ConfigStore

	mu      sync.Mutex
	cached  []string
	fetched time.Time
}

// ConfigStore is the platform config access Keys needs.
type ConfigStore interface {
	// Get returns a value and when it last changed; found is false when the key does not exist.
	Get(ctx context.Context, key string) (value string, updated time.Time, found bool, err error)
	// Put writes a value; an empty value deletes the key.
	Put(ctx context.Context, key, value, description string) error
}

// NewKeys builds the key store.
func NewKeys(cfg ConfigStore) *Keys { return &Keys{cfg: cfg} }

// Active returns the keys a signature may match: the current one first, then the previous one.
func (k *Keys) Active(ctx context.Context) ([]string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.cached != nil && time.Since(k.fetched) < keyCacheTTL {
		return k.cached, nil
	}
	cur, err := k.value(ctx, KeyConfigKey)
	if err != nil {
		return nil, err
	}
	prev, err := k.value(ctx, PreviousKeyConfigKey)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, 2)
	for _, v := range []string{cur, prev} {
		if v != "" {
			keys = append(keys, v)
		}
	}
	k.cached, k.fetched = keys, time.Now()
	return keys, nil
}

// Configured reports whether a signing key exists, and when the current one was set.
func (k *Keys) Configured(ctx context.Context) (bool, time.Time) {
	_, updated, found, err := k.cfg.Get(ctx, KeyConfigKey)
	if err != nil || !found {
		return false, time.Time{}
	}
	return true, updated
}

// Rotate creates a new random key, stores it (the old one becomes the previous key) and applies it
// to the mail server. If applying fails the stored keys are put back so the mail server and the
// receiver never disagree. It returns the time of the switch.
func (k *Keys) Rotate(ctx context.Context, apply Applier) (time.Time, error) {
	if apply == nil {
		return time.Time{}, errors.New("maildelivery: no applier configured")
	}
	newKey, err := generateKey()
	if err != nil {
		return time.Time{}, err
	}
	oldKey, err := k.value(ctx, KeyConfigKey)
	if err != nil {
		return time.Time{}, err
	}
	oldPrev, err := k.value(ctx, PreviousKeyConfigKey)
	if err != nil {
		return time.Time{}, err
	}
	if err := k.store(ctx, PreviousKeyConfigKey, oldKey); err != nil {
		return time.Time{}, err
	}
	if err := k.store(ctx, KeyConfigKey, newKey); err != nil {
		return time.Time{}, err
	}
	k.invalidate()
	if err := apply(ctx, newKey); err != nil {
		// Roll back with a fresh context: the request may have been cancelled mid way.
		rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = k.store(rb, KeyConfigKey, oldKey)
		_ = k.store(rb, PreviousKeyConfigKey, oldPrev)
		k.invalidate()
		return time.Time{}, fmt.Errorf("maildelivery: mail server did not accept the new key: %w", err)
	}
	return time.Now(), nil
}

func (k *Keys) invalidate() {
	k.mu.Lock()
	k.cached = nil
	k.mu.Unlock()
}

func (k *Keys) value(ctx context.Context, key string) (string, error) {
	v, _, _, err := k.cfg.Get(ctx, key)
	return v, err
}

func (k *Keys) store(ctx context.Context, key, value string) error {
	desc := keyDescription
	if key == PreviousKeyConfigKey {
		desc = previousKeyDescription
	}
	return k.cfg.Put(ctx, key, value, desc)
}

// Fingerprint identifies the current key without revealing it ("" when none is set).
func (k *Keys) Fingerprint(ctx context.Context) string {
	cur, err := k.value(ctx, KeyConfigKey)
	if err != nil {
		return ""
	}
	return platformkey.Fingerprint([]byte(cur))
}

// generateKey returns 32 random bytes, base64 encoded.
func generateKey() (string, error) {
	buf, err := platformkey.Random(32)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
