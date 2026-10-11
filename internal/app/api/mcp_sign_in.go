package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
)

// An MCP client signs a person in the way a browser application would: it
// sends them to Weave, they sign in on the console's own page and allow the
// client by name, and the client receives a session of its own. The session
// is good for the MCP entry only and can be taken back at any time.

const (
	mcpTokenPrefix = "wv_mcp_"
	mcpSessionTTL  = externalSessionTTL
	mcpCodeTTL     = time.Minute
	mcpEntryPath   = "/v1/mcp"
)

func (s *Server) registerMCPSignIn(auth *echo.Group) {
	// Clients look for either spelling of the two descriptions.
	for _, suffix := range []string{"", mcpEntryPath} {
		s.Echo.GET("/.well-known/oauth-protected-resource"+suffix, s.handleMCPResource)
		s.Echo.GET("/.well-known/oauth-authorization-server"+suffix, s.handleMCPAuthorizationServer)
	}
	s.Echo.POST("/v1/mcp/oauth/register", s.handleMCPRegisterClient)
	s.Echo.GET("/v1/mcp/oauth/authorize", s.handleMCPAuthorize)
	s.Echo.POST("/v1/mcp/oauth/token", s.handleMCPToken)
	auth.GET("/mcp/oauth/consent", s.handleMCPConsentRequest)
	auth.POST("/mcp/oauth/consent", s.handleMCPConsent)
	auth.GET("/mcp/sessions", s.handleListMCPSessions)
	auth.DELETE("/mcp/sessions/:id", s.handleRevokeMCPSession)
}

func requestOrigin(request *http.Request) string {
	if adminRequestSecure(request) {
		return "https://" + request.Host
	}
	return "http://" + request.Host
}

func (s *Server) handleMCPResource(c echo.Context) error {
	origin := requestOrigin(c.Request())
	return c.JSON(http.StatusOK, map[string]any{"resource": origin + mcpEntryPath, "authorization_servers": []string{origin}})
}

func (s *Server) handleMCPAuthorizationServer(c echo.Context) error {
	origin := requestOrigin(c.Request())
	return c.JSON(http.StatusOK, map[string]any{
		"issuer":                                origin,
		"authorization_endpoint":                origin + "/v1/mcp/oauth/authorize",
		"token_endpoint":                        origin + "/v1/mcp/oauth/token",
		"registration_endpoint":                 origin + "/v1/mcp/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// mcpRedirect accepts where a sign-in may return to: this machine, or an
// HTTPS address.
func mcpRedirect(raw string) (*url.URL, bool) {
	parsed, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || parsed.Host == "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, false
	}
	host := parsed.Hostname()
	loopback := host == "127.0.0.1" || host == "localhost" || host == "::1"
	return parsed, parsed.Scheme == "https" || (parsed.Scheme == "http" && loopback)
}

// mcpRedirectRegistered matches a requested address against the registered
// ones. A native client listens on whatever local port is free, so the port of
// a loopback address is not compared.
func mcpRedirectRegistered(registered []string, requested string) bool {
	want, ok := mcpRedirect(requested)
	if !ok {
		return false
	}
	for _, item := range registered {
		have, ok := mcpRedirect(item)
		if !ok || have.Scheme != want.Scheme || have.Path != want.Path || have.RawQuery != want.RawQuery {
			continue
		}
		if (have.Scheme == "http" && have.Hostname() == want.Hostname()) || have.Host == want.Host {
			return true
		}
	}
	return false
}

func mcpSecret(prefix string) (string, string) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	secret := prefix + base64.RawURLEncoding.EncodeToString(raw)
	return secret, mcpDigest(secret)
}

func mcpDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func (s *Server) handleMCPRegisterClient(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
	var request struct {
		Name      string   `json:"client_name"`
		Redirects []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 16<<10)).Decode(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = "MCP 客户端"
	}
	if len([]rune(name)) > 80 || len(request.Redirects) == 0 || len(request.Redirects) > 8 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
	}
	for _, redirect := range request.Redirects {
		if _, ok := mcpRedirect(redirect); !ok {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri", "error_description": "redirect addresses must be on this machine or use HTTPS"})
		}
	}
	redirects, _ := json.Marshal(request.Redirects)
	ctx := c.Request().Context()
	// Registrations nobody signed in through are not kept.
	_, _ = pool.Exec(ctx, `DELETE FROM weave_mcp_clients c WHERE c.created_at < now() - interval '30 days'
		AND NOT EXISTS (SELECT 1 FROM weave_mcp_sessions m WHERE m.client_id=c.id)`)
	id := "mcp_" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_mcp_clients(id,name,redirect_uris) VALUES($1,$2,$3)`, id, name, redirects); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "server_error"})
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"client_id": id, "client_name": name, "redirect_uris": request.Redirects,
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code"}, "response_types": []string{"code"},
	})
}

type mcpClient struct {
	Name      string
	Redirects []string
}

func (s *Server) mcpClient(ctx context.Context, id string) (mcpClient, bool) {
	pool := s.GetPool()
	var client mcpClient
	var redirects []byte
	if pool == nil || id == "" || pool.QueryRow(ctx, `SELECT name,redirect_uris FROM weave_mcp_clients WHERE id=$1`, id).Scan(&client.Name, &redirects) != nil {
		return client, false
	}
	return client, json.Unmarshal(redirects, &client.Redirects) == nil
}

func mcpRedirectTo(redirect string, values url.Values) string {
	parsed, _ := url.Parse(redirect)
	query := parsed.Query()
	for key, value := range values {
		if value[0] != "" {
			query.Set(key, value[0])
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// handleMCPAuthorize is where a client sends the person's browser. A request
// that names a known client and one of its own addresses goes on to the
// console, which signs the person in and asks them to allow it.
func (s *Server) handleMCPAuthorize(c echo.Context) error {
	query := c.QueryParams()
	client, known := s.mcpClient(c.Request().Context(), query.Get("client_id"))
	if !known || !mcpRedirectRegistered(client.Redirects, query.Get("redirect_uri")) {
		// Nothing is sent to an address the client did not register.
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "unknown client or redirect address"})
	}
	if query.Get("response_type") != "code" || query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		return c.Redirect(http.StatusFound, mcpRedirectTo(query.Get("redirect_uri"), url.Values{"error": {"invalid_request"}, "state": {query.Get("state")}}))
	}
	forward := url.Values{}
	for _, key := range []string{"client_id", "redirect_uri", "code_challenge", "state"} {
		forward.Set(key, query.Get(key))
	}
	return c.Redirect(http.StatusFound, "/admin/connect?"+forward.Encode())
}

// handleMCPConsentRequest tells the console's page which client is asking.
func (s *Server) handleMCPConsentRequest(c echo.Context) error {
	client, known := s.mcpClient(c.Request().Context(), c.QueryParam("client_id"))
	if !known || !mcpRedirectRegistered(client.Redirects, c.QueryParam("redirect_uri")) {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "mcp_client_unknown"})
	}
	redirect, _ := mcpRedirect(c.QueryParam("redirect_uri"))
	// The page shows the person which account they are about to let the client use.
	account := ""
	if pool := s.GetPool(); pool != nil {
		_ = pool.QueryRow(c.Request().Context(), `SELECT COALESCE(NULLIF(display_name,''), username) FROM weave_users WHERE id=$1 AND tenant_id=$2`, getUserID(c), getTenant(c)).Scan(&account)
	}
	return c.JSON(http.StatusOK, map[string]any{"client_name": client.Name, "returns_to": redirect.Hostname(), "account_name": account})
}

// handleMCPConsent records the signed-in person's answer and says where their
// browser goes next.
func (s *Server) handleMCPConsent(c echo.Context) error {
	var request struct {
		Client    string `json:"client_id"`
		Redirect  string `json:"redirect_uri"`
		Challenge string `json:"code_challenge"`
		State     string `json:"state"`
		Allow     bool   `json:"allow"`
	}
	if err := c.Bind(&request); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid_request"})
	}
	ctx := c.Request().Context()
	client, known := s.mcpClient(ctx, request.Client)
	if !known || !mcpRedirectRegistered(client.Redirects, request.Redirect) || len(request.Challenge) < 43 || len(request.Challenge) > 128 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "mcp_client_unknown"})
	}
	if !request.Allow {
		return c.JSON(http.StatusOK, map[string]string{"redirect_to": mcpRedirectTo(request.Redirect, url.Values{"error": {"access_denied"}, "state": {request.State}})})
	}
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
	source, _ := c.Get(identitySourceContextKey).(string)
	sets, _ := c.Get(forgePermissionSetsContextKey).([]string)
	permissionSets, _ := json.Marshal(append([]string{}, sets...))
	code, digest := mcpSecret("")
	_, _ = pool.Exec(ctx, `DELETE FROM weave_mcp_sessions WHERE token_digest IS NULL AND code_expires_at < now() - interval '1 hour'`)
	if _, err := pool.Exec(ctx, `INSERT INTO weave_mcp_sessions(id,workspace_id,user_id,client_id,client_name,role,identity_source,permission_sets,code_digest,code_challenge,redirect_uri,code_expires_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, uuid.New(), getTenant(c), getUserID(c), request.Client, client.Name, firstRole(c), source, permissionSets,
		digest, request.Challenge, request.Redirect, time.Now().Add(mcpCodeTTL)); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "server_error"})
	}
	return c.JSON(http.StatusOK, map[string]string{"redirect_to": mcpRedirectTo(request.Redirect, url.Values{"code": {code}, "state": {request.State}})})
}

// handleMCPToken exchanges the one-time code for the client's session.
func (s *Server) handleMCPToken(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	refused := func(reason string) error {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": reason})
	}
	field := c.FormValue
	if strings.HasPrefix(c.Request().Header.Get(echo.HeaderContentType), echo.MIMEApplicationJSON) {
		body := map[string]string{}
		_ = json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 16<<10)).Decode(&body)
		field = func(name string) string { return body[name] }
	}
	if field("grant_type") != "authorization_code" {
		return refused("unsupported_grant_type")
	}
	verifier := field("code_verifier")
	pool := s.GetPool()
	if pool == nil || field("code") == "" || len(verifier) < 43 || len(verifier) > 128 {
		return refused("invalid_grant")
	}
	challenge := sha256.Sum256([]byte(verifier))
	token, digest := mcpSecret(mcpTokenPrefix)
	expires := time.Now().Add(mcpSessionTTL)
	// One statement spends the code: a second exchange, a late one, another
	// client, another return address or the wrong verifier all find nothing.
	tag, err := pool.Exec(c.Request().Context(), `UPDATE weave_mcp_sessions SET token_digest=$1, expires_at=$2, code_digest=NULL, code_challenge=NULL, code_expires_at=NULL
		WHERE code_digest=$3 AND token_digest IS NULL AND code_expires_at > now() AND client_id=$4 AND redirect_uri=$5 AND code_challenge=$6`,
		digest, expires, mcpDigest(field("code")), field("client_id"), field("redirect_uri"), base64.RawURLEncoding.EncodeToString(challenge[:]))
	if err != nil || tag.RowsAffected() != 1 {
		return refused("invalid_grant")
	}
	return c.JSON(http.StatusOK, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": int(mcpSessionTTL.Seconds())})
}

// mcpSessionMiddleware lets a client's session stand where a console session
// would, on the MCP entry and nowhere else. The person's access is what was
// verified when they allowed the client; whether they may still sign in at all
// is checked against their account on every call.
func (s *Server) mcpSessionMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			request := c.Request()
			entry := request.URL.Path == mcpEntryPath && request.Method == http.MethodPost
			if entry {
				// A client that is not signed in learns here where to sign in.
				c.Response().Before(func() {
					if c.Response().Status == http.StatusUnauthorized {
						c.Response().Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+requestOrigin(request)+`/.well-known/oauth-protected-resource"`)
					}
				})
			}
			const prefix = "Bearer "
			header := request.Header.Get(echo.HeaderAuthorization)
			if !strings.HasPrefix(header, prefix+mcpTokenPrefix) {
				return next(c)
			}
			pool := s.GetPool()
			if !entry || pool == nil {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			}
			var id uuid.UUID
			var workspace, user, role, source string
			var sets []byte
			var idle bool
			err := pool.QueryRow(request.Context(), `SELECT id,workspace_id,user_id,role,identity_source,permission_sets,COALESCE(last_used_at < now() - interval '1 minute', true)
				FROM weave_mcp_sessions WHERE token_digest=$1 AND revoked_at IS NULL AND expires_at > now()`, mcpDigest(strings.TrimPrefix(header, prefix))).
				Scan(&id, &workspace, &user, &role, &source, &sets, &idle)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			}
			var permissionSets []string
			_ = json.Unmarshal(sets, &permissionSets)
			session, err := s.signJWTWithPermissionSets(workspace, user, []string{role}, source, permissionSets, 5*time.Minute)
			if err != nil {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid token"})
			}
			if idle {
				_, _ = pool.Exec(request.Context(), `UPDATE weave_mcp_sessions SET last_used_at=now() WHERE id=$1`, id)
			}
			request.Header.Set(echo.HeaderAuthorization, prefix+session)
			// The call spends this session's allowance, not the account's.
			c.Set(mcpSessionContextKey, id.String())
			return next(c)
		}
	}
}

func (s *Server) handleListMCPSessions(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
	rows, err := pool.Query(c.Request().Context(), `SELECT m.id, m.client_name, m.user_id=$3, COALESCE(NULLIF(u.display_name,''), u.username, ''), m.created_at, m.last_used_at, m.expires_at
		FROM weave_mcp_sessions m LEFT JOIN weave_users u ON u.id=m.user_id AND u.tenant_id=m.workspace_id
		WHERE m.workspace_id=$1 AND m.token_digest IS NOT NULL AND m.revoked_at IS NULL AND m.expires_at > now() AND ($2 OR m.user_id=$3)
		ORDER BY m.created_at DESC LIMIT 200`, getTenant(c), adminRunReader(c), getUserID(c))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "server_error"})
	}
	defer rows.Close()
	type session struct {
		ID       string     `json:"id"`
		Client   string     `json:"client_name"`
		Own      bool       `json:"own"`
		User     string     `json:"user_name"`
		Created  time.Time  `json:"created_at"`
		LastUsed *time.Time `json:"last_used_at,omitempty"`
		Expires  time.Time  `json:"expires_at"`
	}
	sessions := []session{}
	for rows.Next() {
		var item session
		var id uuid.UUID
		if err := rows.Scan(&id, &item.Client, &item.Own, &item.User, &item.Created, &item.LastUsed, &item.Expires); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "server_error"})
		}
		item.ID = id.String()
		sessions = append(sessions, item)
	}
	return c.JSON(http.StatusOK, map[string]any{"sessions": sessions, "entry": requestOrigin(c.Request()) + mcpEntryPath})
}

// handleRevokeMCPSession ends a session at once. A person ends their own; an
// administrator ends any in the workspace.
func (s *Server) handleRevokeMCPSession(c echo.Context) error {
	pool := s.GetPool()
	id, err := uuid.Parse(c.Param("id"))
	if pool == nil || err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "mcp_session_not_found"})
	}
	tag, err := pool.Exec(c.Request().Context(), `UPDATE weave_mcp_sessions SET revoked_at=now(), revoked_by=$4
		WHERE id=$1 AND workspace_id=$2 AND ($3 OR user_id=$4) AND token_digest IS NOT NULL AND revoked_at IS NULL`, id, getTenant(c), adminRunReader(c), getUserID(c))
	if err != nil || tag.RowsAffected() != 1 {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "mcp_session_not_found"})
	}
	return c.NoContent(http.StatusNoContent)
}
