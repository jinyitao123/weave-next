package api

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

var productCapabilityIDs = []string{
	"team.read",
	"run.read",
	"debug.simulate",
	"debug.sandbox_write",
	"release.publish",
}

type productCapabilityDecision struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

func (s *Server) handleProductCapabilities(c echo.Context) error {
	principal := productPrincipalFromContext(c)
	decisions, err := s.ProductAuthorizer.Check(c.Request().Context(), ProductAuthorizationRequest{
		Principal: principal,
		Actions:   productCapabilityIDs,
		Resource: ProductAuthorizationResource{
			Kind: "weave:product",
			ID:   principal.OrganizationID,
			Attributes: map[string]any{
				"environment": "sandbox",
			},
		},
	})
	if err != nil {
		return c.JSON(http.StatusOK, map[string]any{
			"version": "1", "status": "unavailable",
			"subject": map[string]string{"id": principal.ID, "organizationId": principal.OrganizationID},
			"source":  map[string]string{"kind": "cerbos"}, "evaluatedAt": time.Now().UTC().Format(time.RFC3339Nano),
			"capabilities": unavailableProductCapabilities("权限服务暂时不可用"), "message": "权限服务暂时不可用",
		})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"version": "1", "status": "ready",
		"subject":      map[string]string{"id": principal.ID, "organizationId": principal.OrganizationID},
		"source":       map[string]string{"kind": "cerbos", "policyVersion": decisions.PolicyVersion},
		"evaluatedAt":  time.Now().UTC().Format(time.RFC3339Nano),
		"capabilities": productCapabilityProjection(decisions),
	})
}

func (s *Server) requireProductCapability(action string, attributes map[string]any) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			principal := productPrincipalFromContext(c)
			decision, err := s.ProductAuthorizer.Check(c.Request().Context(), ProductAuthorizationRequest{
				Principal: principal,
				Actions:   []string{action},
				Resource:  ProductAuthorizationResource{Kind: "weave:product", ID: principal.OrganizationID, Attributes: attributes},
			})
			if err != nil {
				return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "product authorization unavailable"})
			}
			if !decision.Allowed[action] {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "product capability denied", "action": action})
			}
			return next(c)
		}
	}
}

func productPrincipalFromContext(c echo.Context) ProductAuthorizationPrincipal {
	roles, _ := c.Get("roles").([]string)
	scopes, _ := c.Get(scopesContextKey).([]string)
	productRoles := []string{}
	if c.Get(authSourceContextKey) != authSourceAPIKey || hasAnyString(scopes, "org", "runs", "admin") {
		productRoles = append(productRoles, "employee")
	}
	developer := hasAnyString(roles, "admin", "owner", "developer")
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		developer = hasString(scopes, "capabilities:manage") || (hasString(roles, "admin") && hasString(scopes, "admin"))
	}
	if developer {
		productRoles = append(productRoles, "developer")
	}
	return ProductAuthorizationPrincipal{ID: getUserID(c), OrganizationID: getTenant(c), Roles: productRoles}
}

func productCapabilityProjection(result ProductAuthorizationResult) []productCapabilityDecision {
	items := make([]productCapabilityDecision, 0, len(productCapabilityIDs))
	for _, id := range productCapabilityIDs {
		if result.Allowed[id] {
			items = append(items, productCapabilityDecision{ID: id, Decision: "allow", Reason: "Cerbos 策略允许此项能力"})
		} else {
			items = append(items, productCapabilityDecision{ID: id, Decision: "deny", Reason: "Cerbos 策略未授予此项能力"})
		}
	}
	return items
}

func unavailableProductCapabilities(reason string) []productCapabilityDecision {
	items := make([]productCapabilityDecision, 0, len(productCapabilityIDs))
	for _, id := range productCapabilityIDs {
		items = append(items, productCapabilityDecision{ID: id, Decision: "unavailable", Reason: reason})
	}
	return items
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
