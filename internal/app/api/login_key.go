package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// The login key is a deployment setting that signs its holder in as the
// workspace administrator on the console and on the MCP entry, without an
// account. A deployment that sets none has the well-known default, which every
// page reminds its user of.

const loginKeyIdentitySource = "login_key"

type loginKeyUser struct {
	ID, Workspace, Name string
}

func (s *Server) isLoginKey(token string) bool {
	if s.Config == nil || s.Config.LoginKey == "" || token == "" || strings.HasPrefix(token, "wv_sk_") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.Config.LoginKey)) == 1
}

// loginKeySession issues a session for the administrator the login key stands
// for: the first administrator of the configured workspace, or of the
// deployment when no workspace is configured.
func (s *Server) loginKeySession(ctx context.Context, ttl time.Duration) (string, loginKeyUser, error) {
	pool := s.GetPool()
	if pool == nil {
		return "", loginKeyUser{}, errors.New("login key store unavailable")
	}
	var user loginKeyUser
	err := pool.QueryRow(ctx, `SELECT id,tenant_id,COALESCE(NULLIF(display_name,''),username) FROM weave_users
		WHERE role='admin' AND NOT disabled AND ($1='' OR tenant_id=$1) ORDER BY created_at,id LIMIT 1`, s.Config.LoginWorkspace).Scan(&user.ID, &user.Workspace, &user.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", user, errors.New("no administrator to sign in as")
	}
	if err != nil {
		return "", user, err
	}
	token, err := s.signJWTWithPermissionSets(user.Workspace, user.ID, []string{"admin"}, loginKeyIdentitySource, nil, ttl)
	return token, user, err
}

// loginKeyMiddleware lets the login key stand where a session would: it swaps
// the key for a short session of the administrator before authentication runs,
// so everything after it treats the caller as that administrator.
func (s *Server) loginKeyMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			const prefix = "Bearer "
			header := c.Request().Header.Get(echo.HeaderAuthorization)
			if strings.HasPrefix(header, prefix) && s.isLoginKey(strings.TrimPrefix(header, prefix)) {
				session, _, err := s.loginKeySession(c.Request().Context(), 5*time.Minute)
				if err != nil {
					return c.JSON(http.StatusUnauthorized, map[string]string{"error": "login key has no administrator to sign in as"})
				}
				c.Request().Header.Set(echo.HeaderAuthorization, prefix+session)
			}
			return next(c)
		}
	}
}

// defaultLoginKeySession reports a session opened with the default login key.
func (s *Server) defaultLoginKeySession(c echo.Context) bool {
	source, _ := c.Get(identitySourceContextKey).(string)
	return source == loginKeyIdentitySource && s.Config != nil && s.Config.LoginKeyDefault
}
