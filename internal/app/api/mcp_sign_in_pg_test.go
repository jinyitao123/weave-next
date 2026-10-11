package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/kernel/config"
)

func TestMCPClientSignsAPersonInAndTheSessionCanBeTakenBackRealPG(t *testing.T) {
	s, pool := newTeamDispatchTestServer(t)
	s.Echo, s.UserStore = echo.New(), users.NewStore(pool)
	s.Config = &config.Config{JWTSecret: "mcp-sign-in-test", LoginKey: "admin", LoginKeyDefault: true}
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	defer host.Close()
	ctx := context.Background()
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(method, path, bearer, contentType, body string) (int, map[string]any, http.Header) {
		t.Helper()
		req, _ := http.NewRequest(method, host.URL+path, strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if bearer != "" {
			// The console's own pages: a session, the console header and this origin.
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set(adminRequestHeader, "1")
			req.Header.Set("Origin", host.URL)
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		return resp.StatusCode, decoded, resp.Header
	}
	call := func(method, path, bearer, body string) (int, map[string]any, http.Header) {
		return send(method, path, bearer, "application/json", body)
	}
	tool := func(bearer, name, arguments string) (int, bool) {
		status, body, _ := call("POST", "/v1/mcp", bearer, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+arguments+`}}`)
		result, _ := body["result"].(map[string]any)
		return status, result != nil && result["isError"] != true
	}

	// A client that is not signed in is told where signing in is described.
	status, _, header := call("POST", "/v1/mcp", "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != http.StatusUnauthorized || header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+host.URL+`/.well-known/oauth-protected-resource"` {
		t.Fatalf("unauthenticated entry: %d %q", status, header.Get("WWW-Authenticate"))
	}
	if status, body, _ := call("GET", "/.well-known/oauth-protected-resource", "", ""); status != http.StatusOK || body["resource"] != host.URL+"/v1/mcp" {
		t.Fatalf("resource description: %d %v", status, body)
	}
	status, description, _ := call("GET", "/.well-known/oauth-authorization-server", "", "")
	if status != http.StatusOK || description["authorization_endpoint"] != host.URL+"/v1/mcp/oauth/authorize" || description["token_endpoint"] != host.URL+"/v1/mcp/oauth/token" ||
		description["registration_endpoint"] != host.URL+"/v1/mcp/oauth/register" {
		t.Fatalf("sign-in description: %d %v", status, description)
	}

	// A client registers by name with the addresses a sign-in may return to.
	for _, redirect := range []string{"http://attacker.example/callback", "ftp://127.0.0.1/callback", "https://app.example/callback#fragment"} {
		if status, _, _ := call("POST", "/v1/mcp/oauth/register", "", `{"client_name":"x","redirect_uris":["`+redirect+`"]}`); status != http.StatusBadRequest {
			t.Fatalf("registration returning to %s: %d", redirect, status)
		}
	}
	status, registered, _ := call("POST", "/v1/mcp/oauth/register", "", `{"client_name":"编码助手","redirect_uris":["http://127.0.0.1:7777/callback"]}`)
	clientID, _ := registered["client_id"].(string)
	if status != http.StatusCreated || clientID == "" {
		t.Fatalf("registration: %d %v", status, registered)
	}
	// The client listens on whichever local port is free.
	redirect := "http://127.0.0.1:53124/callback"
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	request := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"client-state"}}

	// The browser is sent on to the console only for a known client and one of its own addresses.
	status, _, header = call("GET", "/v1/mcp/oauth/authorize?"+request.Encode(), "", "")
	forwarded, _ := url.Parse(header.Get("Location"))
	if status != http.StatusFound || forwarded.Path != "/admin/connect" || forwarded.Query().Get("client_id") != clientID || forwarded.Query().Get("state") != "client-state" {
		t.Fatalf("authorize: %d %q", status, header.Get("Location"))
	}
	elsewhere := url.Values{}
	for key, value := range request {
		elsewhere[key] = value
	}
	elsewhere.Set("redirect_uri", "https://attacker.example/callback")
	if status, _, header := call("GET", "/v1/mcp/oauth/authorize?"+elsewhere.Encode(), "", ""); status != http.StatusBadRequest || header.Get("Location") != "" {
		t.Fatalf("authorize returning elsewhere: %d %q", status, header.Get("Location"))
	}
	plain := url.Values{}
	for key, value := range request {
		plain[key] = value
	}
	plain.Set("code_challenge_method", "plain")
	if status, _, header := call("GET", "/v1/mcp/oauth/authorize?"+plain.Encode(), "", ""); status != http.StatusFound || !strings.Contains(header.Get("Location"), "error=invalid_request") || !strings.HasPrefix(header.Get("Location"), redirect) {
		t.Fatalf("authorize without S256: %d %q", status, header.Get("Location"))
	}

	// The console's page names the client, and only a signed-in person answers.
	if status, _, _ := call("GET", "/v1/mcp/oauth/consent?client_id="+clientID+"&redirect_uri="+url.QueryEscape(redirect), "", ""); status != http.StatusUnauthorized {
		t.Fatalf("consent without a session: %d", status)
	}
	if status, body, _ := call("GET", "/v1/mcp/oauth/consent?client_id="+clientID+"&redirect_uri="+url.QueryEscape(redirect), "admin", ""); status != http.StatusOK || body["client_name"] != "编码助手" || body["returns_to"] != "127.0.0.1" {
		t.Fatalf("consent request: %d %v", status, body)
	}
	answer := func(bearer string, allow bool) url.Values {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"client_id": clientID, "redirect_uri": redirect, "code_challenge": challenge, "state": "client-state", "allow": allow})
		status, decided, _ := call("POST", "/v1/mcp/oauth/consent", bearer, string(body))
		target, _ := decided["redirect_to"].(string)
		if status != http.StatusOK || !strings.HasPrefix(target, redirect+"?") {
			t.Fatalf("consent answer: %d %v", status, decided)
		}
		parsed, _ := url.Parse(target)
		return parsed.Query()
	}
	if refused := answer("admin", false); refused.Get("error") != "access_denied" || refused.Get("state") != "client-state" || refused.Get("code") != "" {
		t.Fatalf("declined consent: %v", refused)
	}
	exchange := func(code, codeVerifier, returnsTo string) (int, map[string]any) {
		form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {codeVerifier}, "client_id": {clientID}, "redirect_uri": {returnsTo}}
		status, body, _ := send("POST", "/v1/mcp/oauth/token", "", "application/x-www-form-urlencoded", form.Encode())
		return status, body
	}
	allowed := answer("admin", true)
	code := allowed.Get("code")
	if code == "" || allowed.Get("state") != "client-state" {
		t.Fatalf("allowed consent: %v", allowed)
	}
	// Only the client that started the sign-in can finish it.
	if status, body := exchange(code, strings.Repeat("w", 50), redirect); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("exchange with another verifier: %d %v", status, body)
	}
	if status, body := exchange(code, verifier, "http://127.0.0.1:53124/other"); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("exchange for another address: %d %v", status, body)
	}
	status, issued := exchange(code, verifier, redirect)
	token, _ := issued["access_token"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, mcpTokenPrefix) || issued["token_type"] != "Bearer" || issued["expires_in"] != float64(14*24*60*60) {
		t.Fatalf("exchange: %d %v", status, issued)
	}
	if status, body := exchange(code, verifier, redirect); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("second exchange of one code: %d %v", status, body)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_mcp_sessions WHERE token_digest=$1 OR code_digest=$2`, token, code).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("the token or the code is stored as issued: %d %v", stored, err)
	}

	// The session works on the MCP entry as the person who allowed it, and nowhere else.
	if status, ok := tool(token, "weave_team_list", `{"purpose":"develop"}`); status != http.StatusOK || !ok {
		t.Fatalf("MCP call with the session: %d %v", status, ok)
	}
	for _, path := range []string{"/v1/admin/session", "/v1/mcp/sessions"} {
		if status, _, _ := call("GET", path, token, ""); status != http.StatusUnauthorized {
			t.Fatalf("session used on %s: %d", path, status)
		}
	}

	// A second person, an ordinary member, signs in from the same client.
	var workspace, admin string
	if err := pool.QueryRow(ctx, `SELECT tenant_id,id FROM weave_users WHERE role='admin' ORDER BY created_at,id LIMIT 1`).Scan(&workspace, &admin); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_users(id,tenant_id,username,password,role,display_name) VALUES('mcp-member',$1,'mcp-member','unused','member','小周')`, workspace); err != nil {
		t.Fatal(err)
	}
	member, err := s.signJWTWithPermissionSets(workspace, "mcp-member", []string{"member"}, "forge", []string{"forge_sales"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	status, memberIssued := exchange(answer(member, true).Get("code"), verifier, redirect)
	memberToken, _ := memberIssued["access_token"].(string)
	if status != http.StatusOK || memberToken == "" {
		t.Fatalf("member exchange: %d %v", status, memberIssued)
	}
	if status, ok := tool(memberToken, "weave_team_list", `{"purpose":"use"}`); status != http.StatusOK || !ok {
		t.Fatalf("member using teams: %d %v", status, ok)
	}
	if status, ok := tool(memberToken, "weave_team_list", `{"purpose":"develop"}`); status != http.StatusOK || ok {
		t.Fatalf("member developing teams must be refused by the tool: %d %v", status, ok)
	}

	// Each person sees their own sessions; an administrator sees the workspace's.
	sessions := func(bearer string) []map[string]any {
		t.Helper()
		status, body, _ := call("GET", "/v1/mcp/sessions", bearer, "")
		if status != http.StatusOK || body["entry"] != host.URL+"/v1/mcp" {
			t.Fatalf("sessions: %d %v", status, body)
		}
		var listed []map[string]any
		for _, item := range body["sessions"].([]any) {
			listed = append(listed, item.(map[string]any))
		}
		return listed
	}
	mine := sessions(member)
	if len(mine) != 1 || mine[0]["client_name"] != "编码助手" || mine[0]["own"] != true || mine[0]["user_name"] != "小周" || mine[0]["last_used_at"] == nil {
		t.Fatalf("member's sessions: %v", mine)
	}
	everyone := sessions("admin")
	if len(everyone) != 2 {
		t.Fatalf("workspace sessions: %v", everyone)
	}
	var adminSession string
	for _, item := range everyone {
		if item["own"] == true {
			adminSession, _ = item["id"].(string)
		}
	}
	// Nobody ends a session that is not theirs to end.
	if status, _, _ := call("DELETE", "/v1/mcp/sessions/"+adminSession, member, ""); status != http.StatusNotFound {
		t.Fatalf("member ending the administrator's session: %d", status)
	}
	if status, ok := tool(token, "weave_team_list", `{"purpose":"use"}`); status != http.StatusOK || !ok {
		t.Fatalf("administrator's session after the refused attempt: %d %v", status, ok)
	}
	// Taking a session back ends it at once and keeps the record of who did.
	memberSession, _ := mine[0]["id"].(string)
	if status, _, _ := call("DELETE", "/v1/mcp/sessions/"+memberSession, "admin", ""); status != http.StatusNoContent {
		t.Fatalf("administrator ending the member's session: %d", status)
	}
	if status, _ := tool(memberToken, "weave_team_list", `{"purpose":"use"}`); status != http.StatusUnauthorized {
		t.Fatalf("ended session: %d", status)
	}
	var endedBy string
	if err := pool.QueryRow(ctx, `SELECT revoked_by FROM weave_mcp_sessions WHERE id=$1 AND revoked_at IS NOT NULL`, memberSession).Scan(&endedBy); err != nil || endedBy != admin {
		t.Fatalf("record of the ended session: %q %v", endedBy, err)
	}
	if len(sessions(member)) != 0 || len(sessions("admin")) != 1 {
		t.Fatal("an ended session is still listed")
	}
	if status, _, _ := call("DELETE", "/v1/mcp/sessions/"+memberSession, "admin", ""); status != http.StatusNotFound {
		t.Fatalf("ending a session twice: %d", status)
	}

	// A person who may no longer sign in loses their sessions with it, and a session ends on its date.
	status, again := exchange(answer(member, true).Get("code"), verifier, redirect)
	againToken, _ := again["access_token"].(string)
	if status != http.StatusOK {
		t.Fatalf("member signing in again: %d %v", status, again)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_users SET disabled=true WHERE id='mcp-member'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := tool(againToken, "weave_team_list", `{"purpose":"use"}`); status != http.StatusUnauthorized {
		t.Fatalf("session of a disabled account: %d", status)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_mcp_sessions SET expires_at=now() - interval '1 second' WHERE id=$1`, adminSession); err != nil {
		t.Fatal(err)
	}
	if status, _ := tool(token, "weave_team_list", `{"purpose":"use"}`); status != http.StatusUnauthorized {
		t.Fatalf("session past its date: %d", status)
	}
	// A code nobody exchanged in time is worth nothing.
	late := answer("admin", true).Get("code")
	if _, err := pool.Exec(ctx, `UPDATE weave_mcp_sessions SET code_expires_at=now() - interval '1 second' WHERE token_digest IS NULL`); err != nil {
		t.Fatal(err)
	}
	if status, body := exchange(late, verifier, redirect); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("late exchange: %d %v", status, body)
	}
}
