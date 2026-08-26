package chatrequest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time { return c.now }

func TestStoreBeginCreatesRequestAndDetectsReplayConflict(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	clientRequestID := uuid.NewString()

	created, inserted, err := store.Begin(ctx, BeginRequest{
		WorkspaceID: "workspace-1", UserID: "user-1", ClientRequestID: clientRequestID,
		RequestFingerprint: "sha256:first", ProjectID: "project-1", AgentID: "agent-1",
	})
	if err != nil {
		t.Fatalf("begin request: %v", err)
	}
	if !inserted {
		t.Fatalf("first begin inserted = false")
	}
	if created.Status != "admitting" || created.ClientRequestID != clientRequestID {
		t.Fatalf("unexpected created request: %#v", created)
	}

	replayed, inserted, err := store.Begin(ctx, BeginRequest{
		WorkspaceID: "workspace-1", UserID: "user-1", ClientRequestID: clientRequestID,
		RequestFingerprint: "sha256:first", ProjectID: "project-1", AgentID: "agent-1",
	})
	if err != nil {
		t.Fatalf("replay begin: %v", err)
	}
	if inserted {
		t.Fatalf("replay begin inserted = true")
	}
	if replayed.Status != "admitting" {
		t.Fatalf("replayed status = %q, want admitting", replayed.Status)
	}

	_, _, err = store.Begin(ctx, BeginRequest{
		WorkspaceID: "workspace-1", UserID: "user-1", ClientRequestID: clientRequestID,
		RequestFingerprint: "sha256:different", ProjectID: "project-1", AgentID: "agent-1",
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting replay err = %v, want ErrConflict", err)
	}
}

func TestStoreAdmissionRunningAndTerminalTransitions(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	clientRequestID := beginTestRequest(t, ctx, store, "project-1", "agent-1")

	admitted, err := store.MarkAdmitted(ctx, "workspace-1", "user-1", clientRequestID, "session-1", "conversation-1", "message-user-1")
	if err != nil {
		t.Fatalf("mark admitted: %v", err)
	}
	if admitted.Status != "admitted" || admitted.SessionID != "session-1" ||
		admitted.ConversationID != "conversation-1" || admitted.UserMessageID != "message-user-1" {
		t.Fatalf("unexpected admitted request: %#v", admitted)
	}

	running, err := store.MarkRunning(ctx, "workspace-1", "user-1", clientRequestID)
	if err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if running.Status != "running" {
		t.Fatalf("running status = %q, want running", running.Status)
	}

	terminalPayload := json.RawMessage(`{
		"output":"done",
		"stop_reason":"stop",
		"session_id":"session-1",
		"run_id":"run-1",
		"project_id":"project-1",
		"conversation_id":"conversation-1",
		"user_message_id":"message-user-1"
	}`)
	finished, err := store.MarkFinished(ctx, "workspace-1", "user-1", clientRequestID, "completed", "run-1", terminalPayload)
	if err != nil {
		t.Fatalf("mark finished: %v", err)
	}
	if finished.Status != "completed" || finished.RunID != "run-1" {
		t.Fatalf("unexpected finished request: %#v", finished)
	}
	assertTerminalPayloadFields(t, finished.Response)

	afterFailedAttempt, err := store.MarkFinished(ctx, "workspace-1", "user-1", clientRequestID, "failed", "run-2", json.RawMessage(`{"error":"late"}`))
	if err != nil {
		t.Fatalf("late mark finished: %v", err)
	}
	if afterFailedAttempt.Status != "completed" || afterFailedAttempt.RunID != "run-1" {
		t.Fatalf("terminal request was overwritten: %#v", afterFailedAttempt)
	}
}

func TestStoreQueuedReplayAndLatestConversationRequest(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	firstID := "00000000-0000-0000-0000-000000000001"
	secondID := "00000000-0000-0000-0000-000000000002"
	beginSpecificTestRequest(t, ctx, store, firstID, "project-1", "agent-1")
	beginSpecificTestRequest(t, ctx, store, secondID, "project-1", "agent-1")

	if _, err := store.MarkAdmitted(ctx, "workspace-1", "user-1", firstID, "session-1", "conversation-1", "message-user-1"); err != nil {
		t.Fatalf("admit first: %v", err)
	}
	if _, err := store.MarkAdmitted(ctx, "workspace-1", "user-1", secondID, "session-2", "conversation-1", "message-user-2"); err != nil {
		t.Fatalf("admit second: %v", err)
	}
	queued, err := store.MarkQueued(ctx, "workspace-1", "user-1", secondID, "task-1", json.RawMessage(`{"task_id":"task-1"}`))
	if err != nil {
		t.Fatalf("mark queued: %v", err)
	}
	if queued.Status != "queued" || queued.TaskID != "task-1" {
		t.Fatalf("unexpected queued request: %#v", queued)
	}

	latest, err := store.GetLatestForConversation(ctx, "workspace-1", "user-1", "conversation-1")
	if err != nil {
		t.Fatalf("get latest for conversation: %v", err)
	}
	if latest.ClientRequestID != secondID || latest.UserMessageID != "message-user-2" {
		t.Fatalf("latest request = %#v, want second admitted request", latest)
	}
}

func TestStoreBindWorkflowRunIsIdempotentAndDoesNotOverwriteRun(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	clientRequestID := beginTestRequest(t, ctx, store, "project-1", "agent-1")
	if _, err := store.MarkAdmitted(ctx, "workspace-1", "user-1", clientRequestID, "session-1", "conversation-1", "message-user-1"); err != nil {
		t.Fatalf("mark admitted: %v", err)
	}

	bound, err := store.BindWorkflowRun(ctx, "workspace-1", "user-1", clientRequestID, "run-1", "task-1")
	if err != nil {
		t.Fatalf("bind workflow run: %v", err)
	}
	if bound.Status != "running" || bound.RunID != "run-1" || bound.TaskID != "task-1" {
		t.Fatalf("unexpected bound request: %#v", bound)
	}

	replayed, err := store.BindWorkflowRun(ctx, "workspace-1", "user-1", clientRequestID, "run-1", "task-1")
	if err != nil {
		t.Fatalf("replay bind workflow run: %v", err)
	}
	if replayed.Status != "running" || replayed.RunID != "run-1" || replayed.TaskID != "task-1" {
		t.Fatalf("unexpected replayed request: %#v", replayed)
	}

	afterMismatch, err := store.BindWorkflowRun(ctx, "workspace-1", "user-1", clientRequestID, "run-2", "task-2")
	if err != nil {
		t.Fatalf("mismatched bind workflow run: %v", err)
	}
	if afterMismatch.RunID != "run-1" || afterMismatch.TaskID != "task-1" {
		t.Fatalf("workflow identity was overwritten: %#v", afterMismatch)
	}
}

func TestStoreFailOrphanedRunningRequests(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	clientRequestID := beginTestRequest(t, ctx, store, "project-1", "agent-1")
	if _, err := store.MarkAdmitted(ctx, "workspace-1", "user-1", clientRequestID, "session-orphan", "conversation-1", "message-user-1"); err != nil {
		t.Fatalf("mark admitted: %v", err)
	}
	if _, err := store.MarkRunning(ctx, "workspace-1", "user-1", clientRequestID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE weave_chat_requests
		SET updated_at=$1 WHERE workspace_id='workspace-1' AND user_id='user-1' AND client_request_id=$2`,
		store.clock.Now().Add(-30*time.Minute), clientRequestID); err != nil {
		t.Fatalf("age orphaned request: %v", err)
	}
	_, err := store.pool.Exec(ctx, `
		INSERT INTO weave_session_execution_leases (
			workspace_id,user_id,lead_avatar_id,session_id,lease_epoch,state,
			active_run_id,run_snapshot_id,controller_kind,controller_id,yield_kind,
			acquire_event_id,expires_at,created_at,updated_at,closed_at,close_reason
		) VALUES (
			'workspace-1','user-1','agent-1','session-orphan',1,'closed',
			'run-1','snapshot-1','lead_avatar','agent-1','none',
			'event-1',$1,$2,$2,$2,'final_committed'
		)
	`, store.clock.Now().Add(-time.Hour), store.clock.Now().Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("insert closed lease: %v", err)
	}

	reconciled, err := store.FailOrphaned(ctx, 15*time.Minute)
	if err != nil {
		t.Fatalf("fail orphaned: %v", err)
	}
	if reconciled != 1 {
		t.Fatalf("reconciled = %d, want 1", reconciled)
	}
	failed, err := store.Get(ctx, "workspace-1", "user-1", clientRequestID)
	if err != nil {
		t.Fatalf("get reconciled request: %v", err)
	}
	if failed.Status != "failed" || failed.ErrorCode != "execution_failed" {
		t.Fatalf("unexpected reconciled request: %#v", failed)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	pool := testutil.PostgresPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		CREATE TABLE weave_chat_requests (
			workspace_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			client_request_id UUID NOT NULL,
			request_fingerprint TEXT NOT NULL,
			project_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_id TEXT,
			conversation_id TEXT,
			user_message_id TEXT,
			task_id TEXT,
			run_id TEXT,
			status TEXT NOT NULL DEFAULT 'admitting'
				CHECK (status IN ('admitting','admitted','queued','running','yielded','completed','failed')),
			response JSONB,
			error_code TEXT,
			runtime_assignment JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (workspace_id, user_id, client_request_id),
			CONSTRAINT weave_chat_requests_admission_shape_check
				CHECK (
					(session_id IS NULL AND conversation_id IS NULL AND user_message_id IS NULL)
					OR
					(session_id IS NOT NULL AND conversation_id IS NOT NULL AND user_message_id IS NOT NULL)
				)
		);
		CREATE TABLE weave_session_execution_leases (
			workspace_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			lead_avatar_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			lease_epoch BIGINT NOT NULL,
			state TEXT NOT NULL,
			active_run_id TEXT NOT NULL,
			run_snapshot_id TEXT NOT NULL,
			controller_kind TEXT NOT NULL,
			controller_id TEXT NOT NULL,
			yield_kind TEXT NOT NULL,
			resume_token_hash BYTEA,
			yield_generation BIGINT,
			input_schema JSONB,
			acquire_event_id TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			closed_at TIMESTAMPTZ,
			close_reason TEXT,
			PRIMARY KEY (workspace_id, user_id, lead_avatar_id, session_id)
		);
	`)
	if err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	return New(pool, fixedClock{now: time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)})
}

func beginTestRequest(t *testing.T, ctx context.Context, store *Store, projectID, agentID string) string {
	t.Helper()
	clientRequestID := uuid.NewString()
	beginSpecificTestRequest(t, ctx, store, clientRequestID, projectID, agentID)
	return clientRequestID
}

func beginSpecificTestRequest(t *testing.T, ctx context.Context, store *Store, clientRequestID, projectID, agentID string) {
	t.Helper()
	_, _, err := store.Begin(ctx, BeginRequest{
		WorkspaceID: "workspace-1", UserID: "user-1", ClientRequestID: clientRequestID,
		RequestFingerprint: "sha256:" + clientRequestID, ProjectID: projectID, AgentID: agentID,
	})
	if err != nil {
		t.Fatalf("begin %s: %v", clientRequestID, err)
	}
}

func assertTerminalPayloadFields(t *testing.T, raw json.RawMessage) {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	for _, field := range []string{
		"output", "stop_reason", "session_id", "run_id", "project_id", "conversation_id", "user_message_id",
	} {
		if payload[field] == "" || payload[field] == nil {
			t.Fatalf("terminal payload missing %q: %s", field, raw)
		}
	}
}
