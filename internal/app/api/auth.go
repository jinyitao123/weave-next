package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
)

// ── Request / Response types ─────────────────────────────────

// TokenRequest is the input to the dev-mode token endpoint (backward compat).
type TokenRequest struct {
	Tenant string `json:"tenant"`
	UserID string `json:"user_id"`
}

type RegisterRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Tenant   string `json:"tenant"`
}

type UpdateUserRequest struct {
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
}

type CreateAPIKeyRequest struct {
	Name      string     `json:"name"`
	Role      string     `json:"role"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// ── Dev-mode token (backward compat) ─────────────────────────

// handleIssueToken issues a JWT without credentials.
// Only enabled when WEAVE_DEV_MODE=true.
func (s *Server) handleIssueToken(c echo.Context) error {
	if !s.Config.DevMode {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "dev token endpoint disabled in production"})
	}

	var req TokenRequest
	_ = c.Bind(&req)
	if req.Tenant == "" {
		req.Tenant = "default"
	}
	if req.UserID == "" {
		if user := s.resolveDevTokenUser(c, req.Tenant); user != nil {
			token, err := s.signJWT(user.TenantID, user.ID, []string{user.Role})
			if err != nil {
				return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to sign token"})
			}
			return c.JSON(http.StatusOK, map[string]string{"token": token})
		}
		req.UserID = "dev"
	}

	token, err := s.signJWT(req.Tenant, req.UserID, []string{"admin"})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to sign token"})
	}
	return c.JSON(http.StatusOK, map[string]string{"token": token})
}

func (s *Server) resolveDevTokenUser(c echo.Context, tenant string) *users.User {
	if s.UserStore == nil {
		return nil
	}
	if s.Config != nil && s.Config.AdminUser != "" {
		user, err := s.UserStore.GetByUsername(c.Request().Context(), tenant, s.Config.AdminUser)
		if err == nil && !user.Disabled {
			return user
		}
	}
	list, err := s.UserStore.List(c.Request().Context(), tenant)
	if err != nil {
		return nil
	}
	for i := range list {
		if !list[i].Disabled && list[i].Role == "admin" {
			return &list[i]
		}
	}
	for i := range list {
		if !list[i].Disabled {
			return &list[i]
		}
	}
	return nil
}

// ── Register ─────────────────────────────────────────────────

func (s *Server) handleRegister(c echo.Context) error {
	var req RegisterRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.Username == "" || req.Password == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "username and password are required"})
	}

	tenant := "default"

	// Check if this is the first user (auto-admin bootstrap).
	total, err := s.UserStore.CountAll(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "database error"})
	}

	isFirstUser := total == 0

	// If not the first user, require admin auth.
	if !isFirstUser {
		// Check if the caller is authenticated and is admin.
		roles, _ := c.Get("roles").([]string)
		isAdmin := false
		for _, r := range roles {
			if r == "admin" {
				isAdmin = true
				break
			}
		}
		if !isAdmin {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "only admins can register new users"})
		}
		// Use the caller's tenant.
		if t := getTenant(c); t != "" {
			tenant = t
		}
	}

	role := "user"
	if isFirstUser {
		role = "admin"
	}

	displayName := req.DisplayName
	if displayName == "" {
		displayName = req.Username
	}

	user, err := s.UserStore.Create(c.Request().Context(), tenant, req.Username, req.Password, displayName, role)
	if err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "username already taken"})
	}

	return c.JSON(http.StatusCreated, user)
}

// ── Login ────────────────────────────────────────────────────

func (s *Server) handleLogin(c echo.Context) error {
	var req LoginRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.Username == "" || req.Password == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "username and password are required"})
	}
	if req.Tenant == "" {
		req.Tenant = "default"
	}

	user, err := s.UserStore.Authenticate(c.Request().Context(), req.Tenant, req.Username, req.Password)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
	}

	token, err := s.signJWT(user.TenantID, user.ID, []string{user.Role})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to sign token"})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"token": token,
		"user":  user,
	})
}

// ── Refresh ──────────────────────────────────────────────────

func (s *Server) handleRefresh(c echo.Context) error {
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "api keys cannot refresh tokens"})
	}
	tenant := getTenant(c)
	userID := getUserID(c)
	roles, _ := c.Get("roles").([]string)

	token, err := s.signJWT(tenant, userID, roles)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to sign token"})
	}
	return c.JSON(http.StatusOK, map[string]string{"token": token})
}

// ── Me ───────────────────────────────────────────────────────

func (s *Server) handleMe(c echo.Context) error {
	tenant := getTenant(c)
	userID := getUserID(c)

	// API keys represent their owner for membership checks while remaining a
	// distinct authentication source with no account mutation authority.
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		return c.JSON(http.StatusOK, map[string]any{
			"id":         userID,
			"api_key_id": c.Get(apiKeyIDContextKey),
			"tenant_id":  getTenant(c),
			"role":       firstRole(c),
			"source":     "apikey",
		})
	}

	user, err := s.UserStore.GetByID(c.Request().Context(), tenant, userID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}
	return c.JSON(http.StatusOK, user)
}

// handleUpdateMe lets a signed-in human user change their own display name.
func (s *Server) handleUpdateMe(c echo.Context) error {
	userID := getUserID(c)
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "api keys have no account profile"})
	}
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.DisplayName == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "display_name is required"})
	}
	tenant := getTenant(c)
	if err := s.UserStore.UpdateDisplayName(c.Request().Context(), tenant, userID, req.DisplayName); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}
	user, err := s.UserStore.GetByID(c.Request().Context(), tenant, userID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}
	return c.JSON(http.StatusOK, user)
}

// handleChangeMyPassword lets a signed-in human user rotate their own password.
func (s *Server) handleChangeMyPassword(c echo.Context) error {
	userID := getUserID(c)
	if c.Get(authSourceContextKey) == authSourceAPIKey {
		return c.JSON(http.StatusForbidden, map[string]string{"error": "api keys have no account password"})
	}
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "current_password and new_password are required"})
	}
	tenant := getTenant(c)
	user, err := s.UserStore.GetByID(c.Request().Context(), tenant, userID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}
	if _, err := s.UserStore.Authenticate(c.Request().Context(), tenant, user.Username, req.CurrentPassword); err != nil {
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "current password incorrect"})
	}
	if err := s.UserStore.UpdatePassword(c.Request().Context(), tenant, userID, req.NewPassword); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// User management (admin or owner).

func (s *Server) handleListUsers(c echo.Context) error {
	tenant := getTenant(c)
	users, err := s.UserStore.List(c.Request().Context(), tenant)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	if users == nil {
		return c.JSON(http.StatusOK, []struct{}{})
	}
	return c.JSON(http.StatusOK, users)
}

func (s *Server) handleGetUser(c echo.Context) error {
	user, err := s.UserStore.GetByID(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "user not found"})
	}
	return c.JSON(http.StatusOK, user)
}

func (s *Server) handleUpdateUser(c echo.Context) error {
	var req UpdateUserRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}
	if err := s.UserStore.Update(c.Request().Context(), getTenant(c), c.Param("id"), req.DisplayName, req.Role, req.Disabled); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

func (s *Server) handleDeleteUser(c echo.Context) error {
	if err := s.UserStore.Delete(c.Request().Context(), getTenant(c), c.Param("id")); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
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
	if err := s.KeyStore.Delete(c.Request().Context(), getTenant(c), c.Param("id")); err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.NoContent(http.StatusNoContent)
}

// ── Helpers ──────────────────────────────────────────────────

func (s *Server) signJWT(tenant, userID string, roles []string) (string, error) {
	claims := &Claims{
		TenantID: tenant,
		UserID:   userID,
		Roles:    roles,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
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
