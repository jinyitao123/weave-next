package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

func runtimeAdminRequest(t *testing.T, handler echo.HandlerFunc, workspaceID, method, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "/v1/runtimes/"+id, strings.NewReader(body))
	request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	context := echo.New().NewContext(request, recorder)
	context.SetParamNames("id")
	context.SetParamValues(id)
	context.Set("tenant", workspaceID)
	if err := handler(context); err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestRuntimeAdminReadinessFollowsSchedulerRuleRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "runtime-admin-" + uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES($1,$1,$1)`, workspaceID); err != nil {
		t.Fatal(err)
	}
	store := runtimes.NewStore(pool)
	server := &Server{Runtimes: store}
	runtime, token, err := store.Create(ctx, workspaceID, "mac-studio")
	if err != nil {
		t.Fatal(err)
	}
	read := func() runtimeListItem {
		t.Helper()
		recorder := runtimeAdminRequest(t, server.handleGetRuntime, workspaceID, http.MethodGet, runtime.ID, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("detail = %d %s", recorder.Code, recorder.Body.String())
		}
		var view runtimeListItem
		if err := json.Unmarshal(recorder.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	if view := read(); view.Online || view.Accepting || len(view.EngineReadiness) != 0 {
		t.Fatalf("new runtime = %#v", view)
	}

	if err := store.HelloWithCapabilities(ctx, workspaceID, runtime.ID, []string{"claude", "codex"}, []runtimes.EngineCapability{
		{Engine: "claude", BinaryPath: "/fixture/claude", BinaryVersion: "2.1.0", ProtocolVersion: "1", EndpointClass: "fixture", AuthMode: "oauth", Availability: runtimes.EngineAvailabilityReady},
		{Engine: "codex", BinaryPath: "/fixture/codex", BinaryVersion: "0.146.0", ProtocolVersion: "1", EndpointClass: "fixture", AuthMode: "chatgpt", Availability: runtimes.EngineAvailabilityUnavailable, UnavailableReason: "codex_login_required"},
	}, 2); err != nil {
		t.Fatal(err)
	}
	view := read()
	if !view.Online || !view.Accepting || len(view.EngineReadiness) != 2 {
		t.Fatalf("connected runtime = %#v", view)
	}
	claude, codex := view.EngineReadiness[0], view.EngineReadiness[1]
	if claude.Engine != "claude" || !claude.Accepting || claude.BinaryVersion != "2.1.0" || claude.AuthMode != "oauth" {
		t.Fatalf("claude readiness = %#v", claude)
	}
	if codex.Engine != "codex" || codex.Accepting || codex.Reason != "codex_login_required" {
		t.Fatalf("codex readiness = %#v", codex)
	}
	// The page and the scheduler must agree.
	if _, err := store.Select(ctx, workspaceID, "codex", "", ""); !errors.Is(err, runtimes.ErrNoEligibleRuntime) {
		t.Fatalf("scheduler selected an engine the page reports unavailable: %v", err)
	}
	if assignment, err := store.Select(ctx, workspaceID, "claude", "", ""); err != nil || assignment.RuntimeID != runtime.ID {
		t.Fatalf("scheduler rejected an engine the page reports ready: %#v %v", assignment, err)
	}

	rotated := runtimeAdminRequest(t, server.handleRotateRuntimeToken, workspaceID, http.MethodPost, runtime.ID, "")
	var rotation struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rotated.Body.Bytes(), &rotation); err != nil || rotated.Code != http.StatusOK || !strings.HasPrefix(rotation.Token, "rtk_") {
		t.Fatalf("rotate = %d %s", rotated.Code, rotated.Body.String())
	}
	if _, err := store.ValidateToken(ctx, token); !errors.Is(err, runtimes.ErrInvalidRuntimeToken) {
		t.Fatalf("old token still authenticates: %v", err)
	}
	if current, err := store.ValidateToken(ctx, rotation.Token); err != nil || current.ID != runtime.ID {
		t.Fatalf("new token = %#v %v", current, err)
	}

	otherWorkspace := "runtime-admin-other-" + uuid.NewString()
	if recorder := runtimeAdminRequest(t, server.handleGetRuntime, otherWorkspace, http.MethodGet, runtime.ID, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace detail = %d", recorder.Code)
	}
	if recorder := runtimeAdminRequest(t, server.handleRotateRuntimeToken, otherWorkspace, http.MethodPost, runtime.ID, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-workspace rotation = %d", recorder.Code)
	}
	if err := store.Delete(ctx, workspaceID, runtime.ID); err != nil {
		t.Fatal(err)
	}
	if recorder := runtimeAdminRequest(t, server.handleGetRuntime, workspaceID, http.MethodGet, runtime.ID, ""); recorder.Code != http.StatusNotFound {
		t.Fatalf("deleted detail = %d", recorder.Code)
	}
}
