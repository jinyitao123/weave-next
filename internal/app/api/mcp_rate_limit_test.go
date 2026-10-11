package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

func TestMCPAllowanceSeparatesKeysWithTheSameName(t *testing.T) {
	c := echo.New().NewContext(httptest.NewRequest("POST", "/v1/mcp", nil), httptest.NewRecorder())
	c.Set("tenant", "workspace")
	c.Set("user_id", "owner")
	c.Set(authSourceContextKey, authSourceAPIKey)
	c.Set(apiKeyNameContextKey, "编码助手")
	c.Set(apiKeyIDContextKey, "key-a")
	first := mcpCaller(c)
	c.Set(apiKeyIDContextKey, "key-b")
	if second := mcpCaller(c); first == second {
		t.Fatal("two keys with the same name spend one caller's allowance")
	}
	c.Set(mcpSessionContextKey, "session-a")
	if caller := mcpCaller(c); caller != "session:session-a" {
		t.Fatalf("the client's session must take precedence: %s", caller)
	}
}

func TestMCPAllowanceRefillsEvenlyAndIsKeptPerCaller(t *testing.T) {
	limiter := &mcpRateLimiter{}
	start := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for call := 0; call < mcpStartsPerMinute; call++ {
		if wait := limiter.take("session:a#start", mcpStartsPerMinute, start); wait != 0 {
			t.Fatalf("call %d within the allowance had to wait %s", call+1, wait)
		}
	}
	// The allowance is spent: the next start waits for one to come back, six seconds at ten a minute.
	if wait := limiter.take("session:a#start", mcpStartsPerMinute, start); wait != 6*time.Second {
		t.Fatalf("wait once the allowance is spent: %s", wait)
	}
	// Reading has its own allowance, and so has every other caller.
	if wait := limiter.take("session:a", mcpCallsPerMinute, start); wait != 0 {
		t.Fatalf("reading after starts ran out: %s", wait)
	}
	if wait := limiter.take("session:b#start", mcpStartsPerMinute, start); wait != 0 {
		t.Fatalf("another caller: %s", wait)
	}
	// One comes back after the wait, and only one.
	later := start.Add(6 * time.Second)
	if wait := limiter.take("session:a#start", mcpStartsPerMinute, later); wait != 0 {
		t.Fatalf("after waiting: %s", wait)
	}
	if wait := limiter.take("session:a#start", mcpStartsPerMinute, later); wait == 0 {
		t.Fatal("a second call came back with the first")
	}
	// A caller that stayed away comes back to a full allowance, never more than one.
	rested := start.Add(time.Hour)
	for call := 0; call < mcpStartsPerMinute; call++ {
		if wait := limiter.take("session:a#start", mcpStartsPerMinute, rested); wait != 0 {
			t.Fatalf("call %d after a rest had to wait %s", call+1, wait)
		}
	}
	if wait := limiter.take("session:a#start", mcpStartsPerMinute, rested); wait == 0 {
		t.Fatal("a rest earned more than one full allowance")
	}
	if len(limiter.allowances) != 1 {
		t.Fatalf("idle callers are still kept: %d", len(limiter.allowances))
	}
}
