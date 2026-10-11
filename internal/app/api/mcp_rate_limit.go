package api

import (
	"math"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
)

// Each caller of the MCP entry has two allowances a minute: a generous one for
// reading and editing, and a small one for the tools that start work — every
// one of those spends model budget. An agent that loops is told to wait rather
// than left to run up either.

const (
	mcpCallsPerMinute     = 120
	mcpStartsPerMinute    = 10
	mcpSessionContextKey  = "mcp_session"
	mcpLimiterIdleSeconds = 120
)

// mcpStartingTools are the tools that start a team working.
var mcpStartingTools = map[string]bool{"weave_development_trial": true, "weave_work_submit": true, "weave_development_publish": true}

type mcpAllowance struct {
	left float64
	at   time.Time
}

// mcpRateLimiter counts in this process. Several Weave processes behind one
// address each keep their own count.
type mcpRateLimiter struct {
	mu         sync.Mutex
	allowances map[string]*mcpAllowance
	swept      time.Time
}

// take spends one call from the caller's allowance, or reports how long until
// there is one to spend. The allowance refills evenly through the minute.
func (l *mcpRateLimiter) take(caller string, perMinute int, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.allowances == nil {
		l.allowances = map[string]*mcpAllowance{}
	}
	if now.Sub(l.swept) > mcpLimiterIdleSeconds*time.Second {
		// A caller that has been away this long is back to a full allowance anyway.
		for key, allowance := range l.allowances {
			if now.Sub(allowance.at) > mcpLimiterIdleSeconds*time.Second {
				delete(l.allowances, key)
			}
		}
		l.swept = now
	}
	limit, rate := float64(perMinute), float64(perMinute)/60
	allowance := l.allowances[caller]
	if allowance == nil {
		allowance = &mcpAllowance{left: limit, at: now}
		l.allowances[caller] = allowance
	}
	allowance.left = math.Min(limit, allowance.left+now.Sub(allowance.at).Seconds()*rate)
	allowance.at = now
	// A hair of tolerance keeps rounding in the refill from costing a caller a second.
	if allowance.left < 1-1e-9 {
		return time.Duration(math.Ceil((1-allowance.left)/rate)) * time.Second
	}
	allowance.left--
	return 0
}

// mcpCaller names whose allowance a call spends: the client's session where
// there is one, otherwise the key or the account that was presented.
func mcpCaller(c echo.Context) string {
	if session, _ := c.Get(mcpSessionContextKey).(string); session != "" {
		return "session:" + session
	}
	if source, _ := c.Get(identitySourceContextKey).(string); source == loginKeyIdentitySource {
		return "login-key"
	}
	account := getTenant(c) + "/" + getUserID(c)
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		name, _ := c.Get(apiKeyNameContextKey).(string)
		return "key:" + account + "/" + name
	}
	return "account:" + account
}

// mcpThrottle says whether a tool call must wait, and for how long.
func (s *Server) mcpThrottle(c echo.Context, tool string) *mcpEntryError {
	caller, limit := mcpCaller(c), mcpCallsPerMinute
	if mcpStartingTools[tool] {
		caller, limit = caller+"#start", mcpStartsPerMinute
	}
	if wait := s.mcpLimiter.take(caller, limit, time.Now()); wait > 0 {
		return mcpFail("rate_limited", "调用过于频繁，请在 %d 秒后重试", int(wait.Seconds()))
	}
	return nil
}
