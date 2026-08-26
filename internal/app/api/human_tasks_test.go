package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/labstack/echo/v4"
)

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
	server := &Server{OrgStore: org.NewStore(pool)}
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
	if err := server.requireCurrentWorkspaceMember(ctx); err != nil {
		t.Fatalf("write forbidden response: %v", err)
	}
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("revoked membership status = %d, want 403", recorder.Code)
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
