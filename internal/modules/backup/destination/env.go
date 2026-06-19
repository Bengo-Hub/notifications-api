package destination

import (
	"context"
	"crypto/sha256"
	"os"
	"runtime"
)

// getenv returns the env var value or fallback when unset/empty.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// inheritEnv returns a copy of the current process environment, used as the base
// for rclone invocations so it still finds HOME, CA certificates, proxy settings,
// etc. The ephemeral RCLONE_CONFIG_* backend vars are appended on top.
func inheritEnv() []string {
	src := os.Environ()
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// configFileNul returns the platform null device path passed to `rclone --config`
// so rclone neither reads nor writes any persistent config file — all backend
// settings come from RCLONE_CONFIG_* env vars instead. The production runtime is
// Linux (alpine); the Windows branch keeps local `go build`/dev runs honest.
func configFileNul() string {
	if runtime.GOOS == "windows" {
		return "NUL"
	}
	return "/dev/null"
}

// secretKeyCipher is the uniform Cipher used by this service: a single AES-256 key
// derived as sha256(SECRET_KEY). It satisfies the destination.Cipher interface
// without wiring the service's provider-credential KeyProvider, keeping the
// backup-destination subsystem self-contained.
type secretKeyCipher struct {
	key [32]byte
}

// NewSecretKeyCipher derives a 32-byte AES-256 key from the SECRET_KEY env var via
// sha256. PrimaryKey and the sole CandidateKey are that derived key, so configs
// encrypt and decrypt under the same material.
func NewSecretKeyCipher() Cipher {
	return &secretKeyCipher{key: sha256.Sum256([]byte(os.Getenv("SECRET_KEY")))}
}

// PrimaryKey returns the 32-byte sha256(SECRET_KEY) used to encrypt new data.
func (c *secretKeyCipher) PrimaryKey(ctx context.Context) []byte {
	k := c.key
	return k[:]
}

// CandidateKeys returns the single derived key (no rotation history for this
// uniform SECRET_KEY scheme).
func (c *secretKeyCipher) CandidateKeys(ctx context.Context) [][]byte {
	k := c.key
	return [][]byte{k[:]}
}
