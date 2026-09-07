package encryption

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

// SecretKeys are provider setting keys that must be stored encrypted at rest.
var SecretKeys = map[string]bool{
	"api_key": true, "password": true, "auth_token": true, "api_secret": true,
	"auth_id":      true, // Plivo
	"access_token": true, // Meta WhatsApp Cloud API permanent system-user token
	"app_secret":   true, // Meta app secret (webhook signature verification)
	"private_key":  true, // FCM service-account key
}

// IsSecret returns true if the setting key should be encrypted.
func IsSecret(key string) bool { return SecretKeys[key] }

// Encrypt encrypts plaintext with the given key (32 bytes for AES-256). Returns base64-encoded nonce+ciphertext.
func Encrypt(plaintext string, key []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("encryption key must be 32 bytes")
	}
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a base64-encoded nonce+ciphertext with the given key.
func Decrypt(encoded string, key []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("encryption key must be 32 bytes")
	}
	if encoded == "" {
		return "", nil
	}
	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// KeyFromEnv returns a 32-byte key from base64 or hex env value. Empty if not set or invalid.
func KeyFromEnv(b64OrHex string) []byte {
	if b64OrHex == "" {
		return nil
	}
	// Try base64 first
	key, err := base64.StdEncoding.DecodeString(b64OrHex)
	if err == nil && len(key) == 32 {
		return key
	}
	// Try base64 URL encoding
	key, err = base64.URLEncoding.DecodeString(b64OrHex)
	if err == nil && len(key) == 32 {
		return key
	}
	return nil
}
