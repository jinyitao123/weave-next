package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

const productAccessPolicyVersion = "builtin-v1"

type productCapabilityDecision struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (s *Server) handleProductCapabilities(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]any{
		"version":      "1",
		"status":       "ready",
		"subject":      map[string]string{"id": getUserID(c), "organizationId": getTenant(c)},
		"source":       map[string]string{"kind": "weave", "policyVersion": productAccessPolicyVersion},
		"evaluatedAt":  time.Now().UTC().Format(time.RFC3339Nano),
		"capabilities": productCapabilities(c),
	})
}

func productCapabilities(c echo.Context) []productCapabilityDecision {
	roles, _ := c.Get("roles").([]string)
	scopes, _ := c.Get(scopesContextKey).([]string)
	isAPIKey := c.Get(authSourceContextKey) == authSourceAPIKey

	teamRead := !isAPIKey || hasString(scopes, "org") || hasString(scopes, "admin")
	runRead := !isAPIKey || hasString(scopes, "runs") || hasString(scopes, "admin")
	developerRole := hasAnyString(roles, "admin", "owner", "developer")
	manageCapabilities := developerRole
	if isAPIKey {
		manageCapabilities = hasString(scopes, "capabilities:manage") || (hasString(roles, "admin") && hasString(scopes, "admin"))
	}

	return []productCapabilityDecision{
		productDecision("team.read", teamRead, "已授权查看当前组织的团队", "当前凭据没有组织查看范围"),
		productDecision("run.read", runRead, "已授权查看当前组织的运行记录", "当前凭据没有运行查看范围"),
		productDecision("debug.simulate", manageCapabilities, "已授权使用开发者只读模拟", "只读模拟需要开发者权限"),
		{ID: "debug.sandbox_write", Decision: "deny", Reason: "当前组织尚未接入可写沙箱"},
		productDecision("release.publish", manageCapabilities, "已授权发布团队或能力版本", "发布需要开发者权限"),
	}
}

func productDecision(id string, allowed bool, allowedReason, deniedReason string) productCapabilityDecision {
	if allowed {
		return productCapabilityDecision{ID: id, Decision: "allow", Reason: allowedReason}
	}
	return productCapabilityDecision{ID: id, Decision: "deny", Reason: deniedReason}
}

func hasString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func hasAnyString(values []string, expected ...string) bool {
	for _, candidate := range expected {
		if hasString(values, candidate) {
			return true
		}
	}
	return false
}
