package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestDeviceEnrollmentNeedsThePersonsOwnAccountRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('admin','ws','admin','unused','admin'),('alice','ws','alice','unused','member');
	`); err != nil {
		t.Fatal(err)
	}
	s := &Server{Echo: echo.New(), Config: &config.Config{JWTSecret: "device-enrollment-test", LoginKey: "admin", LoginKeyDefault: true},
		UserStore: users.NewStore(pool), Pool: pool, Store: teamDispatchPoolStore{pool: pool}, Runtimes: runtimes.NewStore(pool)}
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	defer host.Close()

	session := func(user, role, source string) string {
		t.Helper()
		token, err := s.signJWTWithPermissionSets("ws", user, []string{role}, source, nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	post := func(path, bearer string, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, host.URL+path, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := host.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		var decoded map[string]any
		_ = json.Unmarshal(data, &decoded)
		return resp.StatusCode, decoded
	}

	alice := session("alice", "member", "forge")
	status, issued := post("/v1/devices/enrollments", alice, map[string]any{})
	if status != http.StatusCreated {
		t.Fatalf("issue: %d %v", status, issued)
	}
	code, _ := issued["code"].(string)
	commands, _ := issued["commands"].(map[string]any)
	origin := host.URL
	if mac, _ := commands["macos"].(string); !strings.Contains(mac, `"`+origin+`/install.sh"`) || !strings.Contains(mac, "--device-code "+code) {
		t.Fatalf("macOS command = %q", mac)
	}
	if win, _ := commands["windows"].(string); !strings.Contains(win, `-DeviceCode "`+code+`"`) {
		t.Fatalf("Windows command = %q", win)
	}

	// Keys never add devices: the deployment login key, nor a console session
	// that signed in with an API key.
	for name, bearer := range map[string]string{"login key": "admin", "api-key console session": session("admin", "admin", adminAPIKeyIdentitySource)} {
		if status, refused := post("/v1/devices/enrollments", bearer, map[string]any{}); status != http.StatusForbidden || refused["code"] != "account_session_required" {
			t.Fatalf("%s: %d %v", name, status, refused)
		}
	}

	exchange := func(code, platform string) (int, map[string]any) {
		return post("/v1/runtime/device-enrollments", "", map[string]any{"code": code, "hostname": "Alice Mac", "platform": platform, "arch": "arm64"})
	}
	if status, refused := exchange(code, "linux"); status != http.StatusBadRequest || refused["code"] != "unsupported_platform" {
		t.Fatalf("linux: %d %v", status, refused)
	}
	status, device := exchange(code, "darwin")
	if status != http.StatusOK || device["name"] != "Alice Mac" || device["token"] == "" {
		t.Fatalf("exchange: %d %v", status, device)
	}
	if runtime, err := s.Runtimes.ValidateToken(ctx, device["token"].(string)); err != nil || runtime.ID != device["runtime_id"] {
		t.Fatalf("exchanged token: %+v %v", runtime, err)
	}
	if status, refused := exchange(code, "darwin"); status != http.StatusConflict || refused["code"] != "enrollment_code_used" {
		t.Fatalf("reuse: %d %v", status, refused)
	}
	if status, refused := exchange("ZZZZ-ZZZZ", "darwin"); status != http.StatusNotFound || refused["code"] != "enrollment_code_invalid" {
		t.Fatalf("unknown: %d %v", status, refused)
	}
	// Through the MCP entry the command names the address the client used.
	rpc := func(bearer string) map[string]any {
		t.Helper()
		status, response := post("/v1/mcp", bearer, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "device_enroll", "arguments": map[string]any{}}})
		result, _ := response["result"].(map[string]any)
		if status != http.StatusOK || result == nil {
			t.Fatalf("device_enroll: %d %v", status, response)
		}
		var answer map[string]any
		_ = json.Unmarshal([]byte(result["content"].([]any)[0].(map[string]any)["text"].(string)), &answer)
		answer["isError"] = result["isError"]
		return answer
	}
	if answer := rpc(alice); answer["isError"] == true || !strings.Contains(answer["commands"].(map[string]any)["macos"].(string), `"`+origin+`/install.sh"`) {
		t.Fatalf("device_enroll over MCP: %v", answer)
	}
	if answer := rpc("admin"); answer["isError"] != true || answer["code"] != "account_session_required" {
		t.Fatalf("device_enroll with the login key: %v", answer)
	}

	// Guessing from one address is cut off.
	limited := false
	for range deviceEnrollmentsPerMinute + 1 {
		if status, _ := exchange("ZZZZ-ZZZZ", "darwin"); status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated guesses were never limited")
	}
}
