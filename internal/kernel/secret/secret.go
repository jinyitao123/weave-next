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
	"strings"
)

const (
	keySize    = 32
	keyEnv     = "WEAVE_SECRET_KEY"
	keyFileEnv = "WEAVE_SECRET_KEY_FILE"
)

// BoundaryToken returns the HMAC token authorizing one agent MCP boundary.
// It returns an empty string when the credential key is unavailable or invalid.
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

// KeyFromEnv loads the credential encryption key from WEAVE_SECRET_KEY or
// WEAVE_SECRET_KEY_FILE. The sources are mutually exclusive so deployment
// mistakes cannot silently select a different key and strand existing
// ciphertext.
func KeyFromEnv() ([]byte, error) {
	value := strings.TrimSpace(os.Getenv(keyEnv))
	path := strings.TrimSpace(os.Getenv(keyFileEnv))
	source := keyEnv
	if value != "" && path != "" {
		return nil, fmt.Errorf("credential encryption key configuration is ambiguous: set only one of %s or %s", keyEnv, keyFileEnv)
	}
	if path != "" {
		source = keyFileEnv
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("read credential encryption key from %s: %w", keyFileEnv, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("read credential encryption key from %s: path must name a regular file", keyFileEnv)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read credential encryption key from %s: %w", keyFileEnv, err)
		}
		value = strings.TrimSpace(string(contents))
	}
	if value == "" {
		return nil, fmt.Errorf("credential encryption key not configured: set %s or %s", keyEnv, keyFileEnv)
	}
	key, err := parseKey(value)
	if err != nil {
		return nil, fmt.Errorf("invalid credential encryption key from %s: %w", source, err)
	}
	return key, nil
}

func parseKey(value string) ([]byte, error) {
	if len(value) == hex.EncodedLen(keySize) {
		if key, err := hex.DecodeString(value); err == nil && len(key) == keySize {
			return key, nil
		}
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("credential encryption key must be 64-character hex or base64-encoded 32 bytes: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("credential encryption key must decode to 32 bytes, got %d", len(key))
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
