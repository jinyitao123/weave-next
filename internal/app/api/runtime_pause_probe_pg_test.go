package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

func TestRuntimePauseStopsNewWorkAndProbeRidesTheHeartbeatRealPG(t *testing.T) {
	ctx := context.Background()
	server, pool := newTeamDispatchTestServer(t)
	server.Pool = pool
	server.Runtimes = runtimes.NewStore(pool)
	server.Echo = echo.New()
	host := server.Echo.Group("/v1/runtime", server.runtimeAuthMiddleware())
	host.POST("/hello", server.handleRuntimeHello)
	host.POST("/heartbeat", server.handleRuntimeHeartbeat)
	host.POST("/claim", server.handleRuntimeClaim)
	node, token, err := server.Runtimes.Create(ctx, "ws", "mac-studio")
	if err != nil {
		t.Fatal(err)
	}
	send := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, _ := json.Marshal(body)
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set(runtimeprotocol.HeaderVersion, runtimeprotocol.ProtocolVersion)
		recorder := httptest.NewRecorder()
		server.Echo.ServeHTTP(recorder, request)
		return recorder
	}
	admin := func(handler echo.HandlerFunc, body string) int {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(body)))
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		c.SetParamNames("id")
		c.SetParamValues(node.ID)
		c.Set("tenant", "ws")
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return recorder.Code
	}
	hello := runtimeprotocol.HostHelloRequest{Versioned: runtimeprotocol.NewVersioned(), Engines: []string{"claude"}, TotalSlots: 1,
		EngineCapabilities: []runtimeprotocol.EngineCapability{{Engine: "claude", BinaryPath: "/fixture/claude", BinaryVersion: "fixture", AuthMode: runtimes.AuthModeOAuth, ProtocolVersion: "1", EndpointClass: "host_configured"}}}
	if got := send("/v1/runtime/hello", hello); got.Code != http.StatusOK {
		t.Fatalf("hello = %d %s", got.Code, got.Body.String())
	}
	reason := func() string {
		t.Helper()
		listed, err := server.Runtimes.Get(ctx, "ws", node.ID)
		if err != nil {
			t.Fatal(err)
		}
		return runtimes.UnavailableReason(*listed, "claude")
	}
	if got := reason(); got != "" {
		t.Fatalf("connected node unavailable: %s", got)
	}

	if code := admin(server.handleSetRuntimePaused, `{}`); code != http.StatusBadRequest {
		t.Fatalf("missing paused = %d", code)
	}
	if code := admin(server.handleSetRuntimePaused, `{"paused":true}`); code != http.StatusNoContent {
		t.Fatalf("pause = %d", code)
	}
	if got := reason(); got != "runtime_paused" {
		t.Fatalf("paused reason = %q", got)
	}
	if got := send("/v1/runtime/claim", runtimeprotocol.ClaimRequest{Versioned: runtimeprotocol.NewVersioned(), WaitSeconds: 0}); got.Code != http.StatusNoContent {
		t.Fatalf("paused claim = %d %s", got.Code, got.Body.String())
	}
	if code := admin(server.handleSetRuntimePaused, `{"paused":false}`); code != http.StatusNoContent {
		t.Fatalf("resume = %d", code)
	}
	if got := reason(); got != "" {
		t.Fatalf("resumed node unavailable: %s", got)
	}

	heartbeat := runtimeprotocol.HostHeartbeatRequest{Versioned: runtimeprotocol.NewVersioned()}
	if got := send("/v1/runtime/heartbeat", heartbeat); got.Code != http.StatusNoContent {
		t.Fatalf("plain heartbeat = %d", got.Code)
	}
	if code := admin(server.handleProbeRuntime, ``); code != http.StatusAccepted {
		t.Fatalf("probe = %d", code)
	}
	got := send("/v1/runtime/heartbeat", heartbeat)
	var response runtimeprotocol.HostHeartbeatResponse
	if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &response) != nil || !response.Probe {
		t.Fatalf("probe heartbeat = %d %s", got.Code, got.Body.String())
	}
	if got := send("/v1/runtime/hello", hello); got.Code != http.StatusOK {
		t.Fatalf("probe hello = %d", got.Code)
	}
	if got := send("/v1/runtime/heartbeat", heartbeat); got.Code != http.StatusNoContent {
		t.Fatalf("heartbeat after probe report = %d", got.Code)
	}
}
