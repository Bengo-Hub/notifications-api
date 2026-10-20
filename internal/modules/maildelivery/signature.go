package maildelivery

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Verify checks the X-Signature header Stalwart adds to a webhook: an HMAC of the raw body with the
// shared key, base64 encoded. Any of the given keys may match, so a key that was just rotated is
// still accepted for the batches already in flight. Comparison is constant time.
func Verify(body []byte, signature string, keys ...string) bool {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return false
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(body)
		sum := mac.Sum(nil)
		if hmac.Equal([]byte(signature), []byte(base64.StdEncoding.EncodeToString(sum))) ||
			hmac.Equal([]byte(strings.ToLower(signature)), []byte(hex.EncodeToString(sum))) {
			return true
		}
	}
	return false
}
