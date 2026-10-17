package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/config"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestDeviceSessionRunsOperationsForItsOwnerRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
		INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('admin','ws','admin','unused','admin'),('alice','ws','alice','unused','member'),('bob','ws','bob','unused','member');
	`); err != nil {
		t.Fatal(err)
	}
	s := &Server{Echo: echo.New(), Config: &config.Config{JWTSecret: "device-session-test", LoginKey: "admin", LoginKeyDefault: true},
		UserStore: users.NewStore(pool), Pool: pool, Store: teamDispatchPoolStore{pool: pool},
		Runtimes: runtimes.NewStore(pool), Tasks: taskqueue.New(pool, nil, time.Minute)}
	s.registerRoutes()
	host := httptest.NewServer(s.Echo)
	defer host.Close()

	send := func(method, path, bearer string, headers map[string]string, body any) (int, map[string]any) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = strings.NewReader(string(raw))
		}
		req, _ := http.NewRequest(method, host.URL+path, reader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		if strings.HasPrefix(path, "/v1/runtime/") {
			req.Header.Set(runtimeprotocol.HeaderVersion, runtimeprotocol.ProtocolVersion)
		}
		for name, value := range headers {
			req.Header.Set(name, value)
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
	session := func(user string) string {
		token, err := s.signJWTWithPermissionSets("ws", user, []string{"member"}, "forge", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	alice, bob := session("alice"), session("bob")

	enrollment, err := s.Runtimes.IssueDeviceEnrollment(ctx, "ws", "alice")
	if err != nil {
		t.Fatal(err)
	}
	device, err := s.Runtimes.EnrollDevice(ctx, enrollment.Code, "Alice Mac", "")
	if err != nil {
		t.Fatal(err)
	}
	facts := runtimeprotocol.DeviceFacts{Platform: "darwin", Available: true, Levels: []string{"observe", "exec"}}
	if status, hello := send(http.MethodPost, "/v1/runtime/hello", device.Token, nil, runtimeprotocol.HostHelloRequest{Versioned: runtimeprotocol.NewVersioned(), Engines: []string{}, TotalSlots: 1, Device: &facts}); status != http.StatusOK {
		t.Fatalf("hello: %d %v", status, hello)
	}

	status, listed := send(http.MethodGet, "/v1/devices", alice, nil, nil)
	devices, _ := listed["devices"].([]any)
	if status != http.StatusOK || len(devices) != 1 || devices[0].(map[string]any)["available"] != true || devices[0].(map[string]any)["platform"] != "darwin" {
		t.Fatalf("alice's devices: %d %v", status, listed)
	}
	if _, listed := send(http.MethodGet, "/v1/devices", bob, nil, nil); len(listed["devices"].([]any)) != 0 {
		t.Fatalf("bob sees alice's device: %v", listed)
	}
	if status, refused := send(http.MethodGet, "/v1/devices", "admin", nil, nil); status != http.StatusForbidden || refused["code"] != "account_session_required" {
		t.Fatalf("login key: %d %v", status, refused)
	}
	if status, refused := send(http.MethodPost, "/v1/devices/"+device.RuntimeID+"/sessions", bob, nil, map[string]any{"reason": "x", "request_id": uuid.NewString()}); status != http.StatusForbidden {
		t.Fatalf("bob opened alice's device: %d %v", status, refused)
	}

	requestID := uuid.NewString()
	status, opened := send(http.MethodPost, "/v1/devices/"+device.RuntimeID+"/sessions", alice, nil, map[string]any{"reason": "整理桌面文件", "request_id": requestID})
	sessionID, _ := opened["session_id"].(string)
	if status != http.StatusOK || opened["state"] != "opening" || sessionID == "" {
		t.Fatalf("open: %d %v", status, opened)
	}
	if _, again := send(http.MethodPost, "/v1/devices/"+device.RuntimeID+"/sessions", alice, nil, map[string]any{"reason": "整理桌面文件", "request_id": requestID}); again["session_id"] != sessionID {
		t.Fatalf("retry opened another session: %v", again)
	}
	if status, conflict := send(http.MethodPost, "/v1/devices/"+device.RuntimeID+"/sessions", alice, nil, map[string]any{"reason": "别的事", "request_id": requestID}); status != http.StatusConflict || conflict["code"] != "request_conflict" {
		t.Fatalf("same request, other reason: %d %v", status, conflict)
	}

	status, claimed := send(http.MethodPost, "/v1/runtime/claim", device.Token, nil, runtimeprotocol.ClaimRequest{Versioned: runtimeprotocol.NewVersioned()})
	var claim runtimeprotocol.ClaimResponse
	raw, _ := json.Marshal(claimed)
	_ = json.Unmarshal(raw, &claim)
	if status != http.StatusOK || claim.DeviceSession == nil || claim.DeviceSession.TaskID != sessionID || claim.DeviceSession.Reason != "整理桌面文件" || claim.Claim != nil {
		t.Fatalf("device claim: %d %v", status, claimed)
	}
	proof := map[string]string{"X-Weave-Task-Epoch": strconv.FormatInt(claim.DeviceSession.ClaimEpoch, 10), "X-Weave-Task-Subject": claim.DeviceSession.Subject.Digest()}
	epoch := claim.DeviceSession.ClaimEpoch
	if _, read := send(http.MethodGet, "/v1/device-sessions/"+sessionID, alice, nil, nil); read["state"] != "ready" {
		t.Fatalf("after claim: %v", read)
	}
	if status, refused := send(http.MethodPost, "/v1/device-sessions/"+sessionID+"/ops", alice, nil, map[string]any{"op_id": uuid.NewString(), "kind": "act", "params": map[string]any{"action": "click"}}); status != http.StatusForbidden || refused["code"] != "level_not_allowed" {
		t.Fatalf("act on an observe/exec device: %d %v", status, refused)
	}

	// The device answers one operation: it takes it and hands a result back.
	answer := func(result map[string]any) chan string {
		done := make(chan string, 1)
		go func() {
			for range 50 {
				_, next := send(http.MethodPost, "/v1/runtime/tasks/"+sessionID+"/device/next", device.Token, proof, runtimeprotocol.DeviceNextRequest{Versioned: runtimeprotocol.NewVersioned(), ClaimEpoch: epoch, WaitSeconds: 2})
				operation, _ := next["operation"].(map[string]any)
				if operation == nil {
					continue
				}
				opID := operation["op_id"].(string)
				if result != nil {
					encoded, _ := json.Marshal(result)
					send(http.MethodPost, "/v1/runtime/tasks/"+sessionID+"/device/ops/"+opID, device.Token, proof, runtimeprotocol.DeviceOperationResult{Versioned: runtimeprotocol.NewVersioned(), ClaimEpoch: epoch, Status: "succeeded", Result: encoded})
				}
				done <- operation["kind"].(string)
				return
			}
			done <- ""
		}()
		return done
	}

	answered := answer(map[string]any{"image": map[string]any{"data": "aGk=", "mime_type": "image/png"}, "capture_id": "c1"})
	opID := uuid.NewString()
	status, shot := send(http.MethodPost, "/v1/device-sessions/"+sessionID+"/ops", alice, nil, map[string]any{"op_id": opID, "kind": "screenshot", "params": map[string]any{}})
	if kind := <-answered; kind != "screenshot" || status != http.StatusOK || shot["status"] != "succeeded" || shot["result"].(map[string]any)["capture_id"] != "c1" {
		t.Fatalf("screenshot: %s %d %v", kind, status, shot)
	}
	var params, result []byte
	if err := pool.QueryRow(ctx, `SELECT params::text, result::text FROM weave_device_operations WHERE session_id=$1 AND op_id=$2`, sessionID, opID).Scan(&params, &result); err != nil || params != nil || result != nil {
		t.Fatalf("delivered params %q and result %q were kept (%v)", params, result, err)
	}

	// Through MCP the screenshot comes back as an image item.
	answered = answer(map[string]any{"image": map[string]any{"data": "aGk=", "mime_type": "image/png"}, "capture_id": "c2"})
	_, rpc := send(http.MethodPost, "/v1/mcp", alice, nil, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "device_screenshot", "arguments": map[string]any{"session_id": sessionID}}})
	<-answered
	content := rpc["result"].(map[string]any)["content"].([]any)
	if first := content[0].(map[string]any); first["type"] != "image" || first["data"] != "aGk=" || !strings.Contains(content[1].(map[string]any)["text"].(string), "c2") {
		t.Fatalf("MCP screenshot content: %v", content)
	}

	// A command handed to the device whose session ends before it answers is
	// unknown, never assumed undone; its audit keeps no argument content.
	taken := answer(nil)
	execID := uuid.NewString()
	execDone := make(chan map[string]any, 1)
	go func() {
		_, outcome := send(http.MethodPost, "/v1/device-sessions/"+sessionID+"/ops", alice, nil, map[string]any{"op_id": execID, "kind": "exec", "params": map[string]any{"argv": []string{"rm", "secret-name"}}})
		execDone <- outcome
	}()
	if kind := <-taken; kind != "exec" {
		t.Fatalf("device took %q", kind)
	}
	if status, closed := send(http.MethodDelete, "/v1/device-sessions/"+sessionID, alice, nil, nil); status != http.StatusOK || closed["state"] != "closing" {
		t.Fatalf("close: %d %v", status, closed)
	}
	if _, next := send(http.MethodPost, "/v1/runtime/tasks/"+sessionID+"/device/next", device.Token, proof, runtimeprotocol.DeviceNextRequest{Versioned: runtimeprotocol.NewVersioned(), ClaimEpoch: epoch}); next["ended"] != true {
		t.Fatalf("device was not told the session ended: %v", next)
	}
	if status, _ := send(http.MethodPost, "/v1/runtime/tasks/"+sessionID+"/device/end", device.Token, proof, runtimeprotocol.DeviceSessionEnd{Versioned: runtimeprotocol.NewVersioned(), ClaimEpoch: epoch, Reason: "closed"}); status != http.StatusNoContent {
		t.Fatalf("device end: %d", status)
	}
	if outcome := <-execDone; outcome["code"] != "unknown_result" {
		t.Fatalf("unanswered exec: %v", outcome)
	}
	var audit string
	if err := pool.QueryRow(ctx, `SELECT audit::text FROM weave_device_operations WHERE session_id=$1 AND op_id=$2`, sessionID, execID).Scan(&audit); err != nil || strings.Contains(audit, "secret-name") || !strings.Contains(audit, `"arguments": 1`) {
		t.Fatalf("exec audit = %s (%v)", audit, err)
	}
	if _, read := send(http.MethodGet, "/v1/device-sessions/"+sessionID, alice, nil, nil); read["state"] != "ended" || read["end_reason"] != "closed" {
		t.Fatalf("after end: %v", read)
	}
	if task, err := s.Tasks.Get(ctx, "ws", sessionID); err != nil || task.Status != taskqueue.StatusCancelled {
		t.Fatalf("session task: %+v %v", task, err)
	}
}
