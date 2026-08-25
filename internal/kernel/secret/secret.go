// Package secret encrypts credentials before they are persisted.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
)

const keySize = 32

// BoundaryToken returns the HMAC token authorizing one agent MCP boundary.
// It returns an empty string when WEAVE_SECRET_KEY is unavailable or invalid.
func BoundaryToken(tenant, agent string, idx int) string {
	key, err := KeyFromEnv()
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s/%s/%d", tenant, agent, idx)
	return hex.EncodeToString(mac.Sum(nil))
}

// MCPGatewayToken returns the HMAC token authorizing one stable registry
// server ID. The domain prefix prevents reuse of legacy index-bound tokens.
func MCPGatewayToken(workspace, agent, serverID string) string {
	key, err := KeyFromEnv()
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "mcp-gateway/v1/%s/%s/%s", workspace, agent, serverID)
	return hex.EncodeToString(mac.Sum(nil))
}

// KeyFromEnv parses WEAVE_SECRET_KEY as 64-character hex or standard base64.
func KeyFromEnv() ([]byte, error) {
	value := os.Getenv("WEAVE_SECRET_KEY")
	if value == "" {
		return nil, fmt.Errorf("credential encryption key not configured: WEAVE_SECRET_KEY is required")
	}

	if len(value) == hex.EncodedLen(keySize) {
		if key, err := hex.DecodeString(value); err == nil && len(key) == keySize {
			return key, nil
		}
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("WEAVE_SECRET_KEY must be 64-character hex or base64-encoded 32 bytes: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("WEAVE_SECRET_KEY must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}

// Seal encrypts plaintext with AES-256-GCM and returns base64(nonce||ciphertext).
func Seal(key, plaintext []byte) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate credential nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a token produced by Seal.
func Open(key []byte, token string) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	sealed, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return nil, fmt.Errorf("decode credential ciphertext: %w", err)
	}
	if len(sealed) < gcm.NonceSize()+gcm.Overhead() {
		return nil, fmt.Errorf("credential ciphertext is too short")
	}
	nonce, ciphertext := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential: %w", err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keySize {
		return nil, fmt.Errorf("credential encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create credential cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create credential GCM: %w", err)
	}
	return gcm, nil
}
