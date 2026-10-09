package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/admissionfence"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

// ── Request / Response types ─────────────────────────────────

type CreateAPIKeyRequest struct {
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// handleMe exposes only the current authenticated identity for installation
// and operator checks. Authentication and the effective role stay owned by
// AuthMiddleware; this endpoint cannot select or mutate an account.
func (s *Server) handleMe(c echo.Context) error {
	identity := map[string]any{
		"id": getUserID(c), "tenant_id": getTenant(c),
		"role": firstRole(c), "source": c.Get(authSourceContextKey),
	}
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		identity["api_key_id"] = c.Get(apiKeyIDContextKey)
	}
	return c.JSON(http.StatusOK, identity)
}

// ── API Key management (admin only) ─────────────────────────

func (s *Server) handleCreateAPIKey(c echo.Context) error {
	var req CreateAPIKeyRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.Name == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "name is required"})
	}
	if req.Role == "" {
		req.Role = "service"
	}

	tenant := getTenant(c)
	createdBy := getUserID(c)

	key, rawKey, err := s.KeyStore.Create(c.Request().Context(), tenant, req.Name, req.Role, createdBy, req.Scopes, req.ExpiresAt)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusCreated, map[string]any{
		"id":            key.ID,
		"name":          key.Name,
		"role":          key.Role,
		"scopes":        key.Scopes,
		"owner_user_id": key.OwnerUserID,
		"key":           rawKey,
		"expires_at":    key.ExpiresAt,
		"created_at":    key.CreatedAt,
	})
}

func (s *Server) handleListAPIKeys(c echo.Context) error {
	tenant := getTenant(c)
	keys, err := s.KeyStore.List(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, keys)
}

func (s *Server) handleDeleteAPIKey(c echo.Context) error {
	intent, err := newAccessChangeIntent(c, "api_key.delete", c.Param("id"), map[string]string{"key_id": c.Param("id")}, []admissionfence.Resource{admissionfence.Actor(execution.Subject{ServiceID: "api-key:" + c.Param("id")})}, nil, nil)
	if err != nil {
		return err
	}
	result, err := s.applyAccessChange(c.Request().Context(), intent, func(ctx context.Context, tx pgx.Tx) (json.RawMessage, error) {
		return json.RawMessage(`{}`), s.KeyStore.DeleteTx(ctx, tx, getTenant(c), c.Param("id"))
	})
	return writeAccessChangeOutcome(c, result, err, http.StatusNoContent)
}

// ── Helpers ──────────────────────────────────────────────────

func (s *Server) signJWTWithPermissionSets(tenant, userID string, roles []string, identitySource string, permissionSets []string, ttl time.Duration) (string, error) {
	claims := &Claims{
		TenantID:       tenant,
		UserID:         userID,
		Roles:          roles,
		IdentitySource: identitySource,
		PermissionSets: permissionSets,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(s.Config.JWTSecret))
}

func firstRole(c echo.Context) string {
	if roles, ok := c.Get("roles").([]string); ok && len(roles) > 0 {
		return roles[0]
	}
	return ""
}
