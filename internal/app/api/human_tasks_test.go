package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/app/apikeys"
	"github.com/jinyitao123/weave/internal/app/users"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"

	orgstore "github.com/jinyitao123/weave/internal/app/org"
	"github.com/labstack/echo/v4"
)

func TestAPIKeyOwnerPassesLiveHumanTaskMembershipRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "human-key-owner-" + uuid.NewString()
	userStore := users.NewStore(pool)
	owner, err := userStore.Create(ctx, workspaceID, "owner", "password", "Owner", "admin")
	if err != nil {
		t.Fatal(err)
	}
	keyStore := apikeys.NewStore(pool)
	_, rawKey, err := keyStore.Create(ctx, workspaceID, "codex", "admin", owner.ID, []string{"runs"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{OrgStore: orgstore.NewStore(pool)}
	e := echo.New()
	e.GET("/v1/human-tasks", func(c echo.Context) error {
		if getUserID(c) != owner.ID {
			return c.JSON(http.StatusForbidden, map[string]string{"error": "api key owner identity missing"})
		}
		if err := server.requireCurrentWorkspaceMember(c); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}, AuthMiddleware("unused", func() *apikeys.Store { return keyStore }, func() *users.Store { return userStore }), RequireScope("runs"))

	request := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/human-tasks", nil)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+rawKey)
		e.ServeHTTP(recorder, req)
		return recorder
	}
	if recorder := request(); recorder.Code != http.StatusNoContent {
		t.Fatalf("owner key status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if err := server.OrgStore.RemoveMember(ctx, workspaceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if recorder := request(); recorder.Code != http.StatusForbidden {
		t.Fatalf("revoked owner key status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestHumanTaskCursorRoundTrip(t *testing.T) {
	wantTime := time.Date(2026, 8, 27, 12, 34, 56, 789, time.UTC)
	cursor, err := encodeHumanTaskCursor(wantTime, "run-2")
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}
	gotTime, gotRunID, err := decodeHumanTaskCursor(cursor)
	if err != nil || gotTime == nil || !gotTime.Equal(wantTime) || gotRunID != "run-2" {
		t.Fatalf("cursor round trip: time=%v run=%q err=%v", gotTime, gotRunID, err)
	}
	if _, _, err := decodeHumanTaskCursor("not-base64!"); err == nil {
		t.Fatal("invalid cursor was accepted")
	}
}

func TestHumanTaskMembershipIsRecheckedAgainstDatabase(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO weave_workspaces (id,slug,name) VALUES ('workspace-human','workspace-human','Human');
		INSERT INTO weave_users (id,tenant_id,username,password,role)
		VALUES ('user-human','workspace-human','human','x','user');
		INSERT INTO weave_members (workspace_id,user_id,role)
		VALUES ('workspace-human','user-human','member')
	`); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	server := &Server{OrgStore: orgstore.NewStore(pool)}
	newContext := func() (echo.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/v1/human-tasks", nil), recorder)
		ctx.Set("tenant", "workspace-human")
		ctx.Set("user_id", "user-human")
		return ctx, recorder
	}
	ctx, _ := newContext()
	if err := server.requireCurrentWorkspaceMember(ctx); err != nil {
		t.Fatalf("current member rejected: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM weave_members
		WHERE workspace_id='workspace-human' AND user_id='user-human'`); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}
	ctx, recorder := newContext()
	err := server.requireCurrentWorkspaceMember(ctx)
	var denied *echo.HTTPError
	if !errors.As(err, &denied) || denied.Code != http.StatusForbidden {
		t.Fatalf("revoked membership must stop the handler: %v", err)
	}
	ctx.Echo().HTTPErrorHandler(err, ctx)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("revoked membership status = %d, want 403", recorder.Code)
	}
}

func TestHumanTaskHandlersStopAfterMembershipDenialRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	workspaceID := "human-denied-" + uuid.NewString()
	owner, err := users.NewStore(pool).Create(t.Context(), workspaceID, "owner", "password", "Owner", "admin")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{OrgStore: orgstore.NewStore(pool)}
	if err := server.OrgStore.RemoveMember(t.Context(), workspaceID, owner.ID); err != nil {
		t.Fatal(err)
	}
	for _, unavailable := range []bool{false, true} {
		if unavailable {
			server.OrgStore = nil
		}
		for _, route := range []struct {
			method, path string
			handler      echo.HandlerFunc
		}{
			{http.MethodGet, "/v1/human-tasks", server.handleListHumanTasks},
			{http.MethodGet, "/v1/human-tasks/run-1", server.handleGetHumanTask},
			{http.MethodPost, "/v1/human-tasks/run-1/complete", server.handleCompleteHumanTask},
		} {
			e := echo.New()
			e.Add(route.method, route.path, route.handler, func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					c.Set("tenant", workspaceID)
					c.Set("user_id", owner.ID)
					return next(c)
				}
			})
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, httptest.NewRequest(route.method, route.path, nil))
			status, message := http.StatusForbidden, "current workspace membership required"
			if unavailable {
				status, message = http.StatusServiceUnavailable, "workspace membership unavailable"
			}
			var response map[string]string
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != status || response["error"] != message {
				t.Fatalf("%s %s continued after membership denial: status=%d body=%s error=%v", route.method, route.path, recorder.Code, recorder.Body.String(), err)
			}
		}
	}
}

func TestHumanResumePayloadUsesFixedSchemaAndCanonicalDigest(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"decision":{"type":"string"},"comment":{"type":"string"}},
		"required":["decision"],
		"additionalProperties":false
	}`)
	if _, problems, err := validateAndCanonicalizeHumanPayload(schema, json.RawMessage(`{"comment":"missing"}`)); err != nil || len(problems) == 0 {
		t.Fatalf("invalid payload: problems=%v err=%v", problems, err)
	}
	left, problems, err := validateAndCanonicalizeHumanPayload(schema, json.RawMessage(`{"decision":"approve","comment":"ok"}`))
	if err != nil || len(problems) != 0 {
		t.Fatalf("validate left payload: problems=%v err=%v", problems, err)
	}
	right, problems, err := validateAndCanonicalizeHumanPayload(schema, json.RawMessage(`{"comment":"ok","decision":"approve"}`))
	if err != nil || len(problems) != 0 {
		t.Fatalf("validate right payload: problems=%v err=%v", problems, err)
	}
	leftDigest, rightDigest := sha256.Sum256(left), sha256.Sum256(right)
	if leftDigest != rightDigest || string(left) != string(right) {
		t.Fatalf("canonical payload mismatch: left=%s right=%s", left, right)
	}
}
