package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/labstack/echo/v4"
)

const (
	authSourceContextKey     = "auth_source"
	scopesContextKey         = "scopes"
	apiKeyIDContextKey       = "api_key_id"
	workbenchActorContextKey = "workbench_actor_id"
	authSourceAPIKey         = "apikey"
	authSourceJWT            = "jwt"
)

// Claims holds the JWT claims for Weave authentication.
type Claims struct {
	TenantID string   `json:"tenant_id"`
	UserID   string   `json:"user_id"`
	Roles    []string `json:"roles"`
	jwt.RegisteredClaims
}

// AuthMiddleware validates JWT tokens or API keys and extracts tenant/user info.
// Store getters are called at request time (lazy) so stores can be set after route registration.
func AuthMiddleware(jwtSecret string, keyStoreGetter func() *apikeys.Store, userStoreGetter func() *users.Store) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth := c.Request().Header.Get("Authorization")
			if auth == "" {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "missing authorization header"})
			}

			tokenStr := strings.TrimPrefix(auth, "Bearer ")
			if tokenStr == auth {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid authorization format"})
			}

			// Path 1: API Key (wv_sk_ prefix).
			if strings.HasPrefix(tokenStr, "wv_sk_") {
				ks := keyStoreGetter()
				if ks == nil {
					return c.JSON(http.StatusUnauthorized, map[string]string{"error": "api keys not configured"})
				}
				key, err := ks.Validate(c.Request().Context(), tokenStr)
				if err != nil {
					return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
				}
				setAPIKeyContext(c, key)
				go ks.TouchLastUsed(context.Background(), key.ID)
				return next(c)
			}

			// Path 2: JWT token.
			claims := &Claims{}
			token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
				return []byte(jwtSecret), nil
			})
			if err != nil || !token.Valid {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			}

			user, ok := resolveJWTUser(c.Request().Context(), userStoreGetter, claims)
			if !ok {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			}

			c.Set("tenant", user.TenantID)
			c.Set("user_id", user.ID)
			c.Set("roles", []string{user.Role})
			c.Set(authSourceContextKey, authSourceJWT)

			return next(c)
		}
	}
}

// OptionalAuthMiddleware tries to authenticate but allows unauthenticated requests through.
// Used for endpoints like /register that need auth context when available but allow first-user bootstrap.
func OptionalAuthMiddleware(jwtSecret string, keyStoreGetter func() *apikeys.Store, userStoreGetter func() *users.Store) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			auth := c.Request().Header.Get("Authorization")
			if auth == "" {
				return next(c)
			}

			tokenStr := strings.TrimPrefix(auth, "Bearer ")
			if tokenStr == auth {
				return next(c)
			}

			// Try API Key.
			if strings.HasPrefix(tokenStr, "wv_sk_") {
				if ks := keyStoreGetter(); ks != nil {
					if key, err := ks.Validate(c.Request().Context(), tokenStr); err == nil {
						setAPIKeyContext(c, key)
						go ks.TouchLastUsed(context.Background(), key.ID)
					}
				}
				return next(c)
			}

			// Try JWT.
			claims := &Claims{}
			token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
				return []byte(jwtSecret), nil
			})
			if err == nil && token.Valid {
				if user, ok := resolveJWTUser(c.Request().Context(), userStoreGetter, claims); ok {
					c.Set("tenant", user.TenantID)
					c.Set("user_id", user.ID)
					c.Set("roles", []string{user.Role})
					c.Set(authSourceContextKey, authSourceJWT)
				}
			}

			return next(c)
		}
	}
}

func setAPIKeyContext(c echo.Context, key *apikeys.APIKey) {
	userID := key.OwnerUserID
	actor := strings.TrimSpace(c.Request().Header.Get("X-Weave-Actor-ID"))
	if key.Role == "admin" && validWorkbenchActor(actor) {
		userID = "workbench:" + actor
		c.Set(workbenchActorContextKey, actor)
	}
	if userID == "" {
		// Upgrade compatibility for an old key whose historical creator cannot
		// be resolved. It remains authenticated but cannot impersonate a member.
		userID = "apikey:" + key.ID
	}
	c.Set("tenant", key.TenantID)
	c.Set("user_id", userID)
	c.Set("roles", []string{key.Role})
	c.Set(authSourceContextKey, authSourceAPIKey)
	c.Set(scopesContextKey, key.Scopes)
	c.Set(apiKeyIDContextKey, key.ID)
}

func validWorkbenchActor(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func resolveJWTUser(ctx context.Context, userStoreGetter func() *users.Store, claims *Claims) (*users.User, bool) {
	if claims.TenantID == "" || claims.UserID == "" {
		return nil, false
	}
	if userStoreGetter == nil {
		return &users.User{ID: claims.UserID, TenantID: claims.TenantID, Role: firstClaimRole(claims.Roles)}, true
	}
	us := userStoreGetter()
	if us == nil {
		return &users.User{ID: claims.UserID, TenantID: claims.TenantID, Role: firstClaimRole(claims.Roles)}, true
	}
	user, err := us.GetByID(ctx, claims.TenantID, claims.UserID)
	if err != nil || user.Disabled {
		return nil, false
	}
	return user, true
}

func firstClaimRole(roles []string) string {
	if len(roles) == 0 {
		return ""
	}
	return roles[0]
}

// RequireScope limits API key access to a route group.
func RequireScope(scope string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Get(authSourceContextKey) != authSourceAPIKey {
				return next(c)
			}
			scopes, _ := c.Get(scopesContextKey).([]string)
			if len(scopes) == 0 {
				return next(c)
			}
			for _, allowed := range scopes {
				if allowed == scope {
					return next(c)
				}
			}
			return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient scope"})
		}
	}
}

// RequireRole checks that the authenticated user has the required role.
func RequireRole(role string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			roles, ok := c.Get("roles").([]string)
			if !ok {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "no roles"})
			}
			for _, r := range roles {
				if r == role {
					return next(c)
				}
			}
			return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
		}
	}
}

// RequireAnyRole allows callers that hold at least one of the supplied roles.
func RequireAnyRole(allowedRoles ...string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			roles, ok := c.Get("roles").([]string)
			if !ok {
				return c.JSON(http.StatusForbidden, map[string]string{"error": "no roles"})
			}
			for _, role := range roles {
				for _, allowed := range allowedRoles {
					if role == allowed {
						return next(c)
					}
				}
			}
			return c.JSON(http.StatusForbidden, map[string]string{"error": "insufficient permissions"})
		}
	}
}

// getTenant extracts the tenant ID from the Echo context.
func getTenant(c echo.Context) string {
	if t, ok := c.Get("tenant").(string); ok {
		return t
	}
	return ""
}

// getUserID extracts the user ID from the Echo context.
func getUserID(c echo.Context) string {
	if u, ok := c.Get("user_id").(string); ok {
		return u
	}
	return ""
}
