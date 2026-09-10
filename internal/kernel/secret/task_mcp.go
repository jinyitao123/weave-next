package secret

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

const TaskMCPTokenPrefix = "tmcp1."

// TaskMCPClaims identifies one server under one persisted claim epoch. Lease,
// revocation and binding checks remain server-side on every gateway request.
type TaskMCPClaims struct {
	WorkspaceID   string `json:"workspace_id"`
	TaskID        string `json:"task_id"`
	RuntimeID     string `json:"runtime_id"`
	ServerIndex   int    `json:"server_index"`
	BindingDigest string `json:"binding_digest"`
	ClaimEpoch    int64  `json:"claim_epoch"`
}

func SignTaskMCPToken(claims TaskMCPClaims) (string, error) {
	key, err := KeyFromEnv()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := TaskMCPTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func VerifyTaskMCPToken(token string) (TaskMCPClaims, error) {
	var claims TaskMCPClaims
	invalid := errors.New("invalid task MCP token")
	if len(token) > 4096 || !strings.HasPrefix(token, TaskMCPTokenPrefix) {
		return claims, invalid
	}
	body, signature, ok := strings.Cut(strings.TrimPrefix(token, TaskMCPTokenPrefix), ".")
	if !ok {
		return claims, invalid
	}
	key, err := KeyFromEnv()
	if err != nil {
		return claims, invalid
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(TaskMCPTokenPrefix + body))
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return claims, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return claims, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil || claims.WorkspaceID == "" || claims.TaskID == "" || claims.RuntimeID == "" || claims.ServerIndex < 0 || claims.ClaimEpoch <= 0 || claims.BindingDigest == "" {
		return TaskMCPClaims{}, invalid
	}
	return claims, nil
}
