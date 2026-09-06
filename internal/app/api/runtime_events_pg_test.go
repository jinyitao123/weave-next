package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

func TestRuntimePublicEventsDurableIdentityReplayAndLateAttemptRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces(id,slug,name) VALUES('ws','ws','ws');
 INSERT INTO weave_agents(id,workspace_id,name,role,spec) VALUES('agent','ws','worker','worker','{}');
 INSERT INTO weave_agent_versions(agent_id,workspace_id,version,spec) VALUES('agent','ws',1,'{}');
 INSERT INTO weave_teams(id,workspace_id,name) VALUES('team','ws','team');`); err != nil {
		t.Fatal(err)
	}
	_, err := snapshot.NewStore(pool).Create(ctx, snapshot.TeamRunSnapshot{
		RunID: "snapshot", WorkspaceID: "ws", TeamID: "team", SnapshotSchemaVersion: 2, Mode: "free_collab", LeadAvatarID: "agent", LeadAvatarVersion: 1,
		WorkerVersions: json.RawMessage(`{}`), TeamWorkerSnapshot: json.RawMessage(`[]`), InlineDependencies: json.RawMessage(`{}`), RuntimeAssignment: json.RawMessage(`{}`),
		AdmissionDecision: json.RawMessage(`{"schema_version":1,"team_active":true,"workflow_active":null,"workers_enabled":true,"version_blocked":null,"decided_at":"2026-09-05T00:00:00Z"}`),
		RunAssociations:   json.RawMessage(`{"schema_version":1,"parent_run_id":null,"source_snapshot_id":null,"task_group_id":null}`),
		TriggerSourceV2:   json.RawMessage(`{"schema_version":1,"type":"api","source_ref":"fixture"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_runs(workspace_id,run_id,status,team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,establish_idempotency_key,current_executor_id,created_at,updated_at)
 VALUES('ws','run','running','team','workflow',1,'snapshot','api','source','establish','executor',NOW(),NOW())`); err != nil {
		t.Fatal(err)
	}
	owner, _, err := runtimes.NewStore(pool).Create(ctx, "ws", "owner")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := runtimes.NewStore(pool).Create(ctx, "ws", "other")
	if err != nil {
		t.Fatal(err)
	}
	tasks := taskqueue.New(pool, nil, time.Minute)
	seedTask := func(id string) {
		t.Helper()
		task := &taskqueue.Task{ID: id, WorkspaceID: "ws", Agent: "worker", AgentID: "agent", AgentVersion: 1, IdentityKind: taskqueue.IdentityAgent,
			IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeTeamFreeCollab, RunSnapshotID: "snapshot", Kind: "engine_exec", RuntimeID: owner.ID, Payload: json.RawMessage(`{"engine":"codex","node_id":"research"}`)}
		if err := tasks.Enqueue(ctx, task); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='running',worker_id=$2,started_at=NOW(),lease_expires_at=NOW()+INTERVAL '1 minute' WHERE id=$1`, id, runtimes.RuntimeWorkerID("ws", owner.ID)); err != nil {
			t.Fatal(err)
		}
	}
	seedTask("task-old")
	server := &Server{Pool: pool, Tasks: tasks, teamRunActivities: &teamrun.PGActivityStore{Transactions: pool}}
	post := func(runtime *runtimes.Runtime, taskID string, body any) int {
		data, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		e := echo.New()
		c := e.NewContext(httptest.NewRequest(http.MethodPost, "/v1/runtime/tasks/"+taskID+"/events", bytes.NewReader(data)), rec)
		c.SetParamNames("id")
		c.SetParamValues(taskID)
		c.Set(runtimeContextKey, runtime)
		if err := server.handleRuntimeTaskEvents(c); err != nil {
			e.HTTPErrorHandler(err, c)
		}
		return rec.Code
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	item := func(seq int64, text string) runtimes.PublicEvent {
		return runtimes.PublicEvent{Seq: seq, OccurredAt: at, Event: engine.Event{Kind: "text", Text: text}}
	}
	body := func(events ...runtimes.PublicEvent) any { return map[string]any{"events": events} }
	first := item(1, "old draft")
	if got := post(other, "task-old", body(first)); got != http.StatusNotFound {
		t.Fatalf("other runtime injected progress: %d", got)
	}
	if got := post(owner, "task-old", map[string]any{"run_id": "another-run", "events": []runtimes.PublicEvent{first}}); got != http.StatusBadRequest {
		t.Fatalf("client-selected run accepted: %d", got)
	}
	private := first
	private.Event.Kind = "thinking"
	if got := post(owner, "task-old", body(private)); got != http.StatusBadRequest {
		t.Fatalf("reasoning accepted: %d", got)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if got := post(owner, "task-old", body(first)); got != http.StatusOK {
				t.Errorf("concurrent replay: %d", got)
			}
		}()
	}
	workers.Wait()
	if got := post(owner, "task-old", body(item(1, "rewritten"))); got != http.StatusConflict {
		t.Fatalf("stable identity rewritten: %d", got)
	}
	if got := post(owner, "task-old", body(item(3, "gap"))); got != http.StatusConflict {
		t.Fatalf("sequence gap accepted: %d", got)
	}
	if got := post(owner, "task-old", body(item(2, "rollback"), item(4, "gap"))); got != http.StatusBadRequest {
		t.Fatalf("invalid batch accepted: %d", got)
	}
	if got := post(owner, "task-old", body(item(2, "second"))); got != http.StatusOK {
		t.Fatal(got)
	}
	seedTask("task-new")
	if _, err := pool.Exec(ctx, `UPDATE weave_task_queue SET status='failed',worker_id=NULL,lease_expires_at=NULL,error=$1 WHERE id='task-old'`, taskqueue.RuntimeLeaseExpiredError); err != nil {
		t.Fatal(err)
	}
	if got := post(owner, "task-new", body(item(1, "current draft"))); got != http.StatusOK {
		t.Fatal(got)
	}
	// The first journal arrives after a retry has started. It remains an old
	// physical task's evidence, never the retry's current message.
	end := runtimes.PublicEvent{Seq: 3, OccurredAt: at, Event: engine.Event{Kind: "stream_end"}, Truncated: true}
	if got := post(owner, "task-old", body(end)); got != http.StatusOK {
		t.Fatal(got)
	}
	if got := post(owner, "task-old", body(item(4, "after end"))); got != http.StatusConflict {
		t.Fatalf("ended stream appended: %d", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET status='cancelled',current_executor_id=NULL,error_code='team_run_cancelled',updated_at=NOW(),terminal_at=NOW() WHERE run_id='run';
 UPDATE weave_task_queue SET status='cancelled',worker_id=NULL,lease_expires_at=NULL,result='{"output":"retained"}' WHERE id='task-new'`); err != nil {
		t.Fatal(err)
	}
	if got := post(owner, "task-new", body(item(2, "late captured text"))); got != http.StatusOK {
		t.Fatal(got)
	}
	for range 2 {
		if got := post(owner, "task-old", body(end)); got != http.StatusOK {
			t.Fatal(got)
		}
	}
	events, err := server.teamRunActivities.List(ctx, "ws", "run", 100)
	if err != nil || len(events) != 5 {
		t.Fatalf("event replay duplicated records: count=%d err=%v", len(events), err)
	}
	for _, event := range events {
		if event.RunID != "run" || event.MemberID != "agent" || event.NodeID != "research" {
			t.Fatalf("wrong server-derived binding: %+v", event)
		}
	}
	members := []runActivityMember{{AgentID: "agent", Stages: []runActivityMemberStage{{NodeID: "research", Status: "cancelled", OutputRefs: []string{"saved"}}}}}
	completeness := map[string]string{"activity_events": "complete"}
	server.projectRunPublicEvents(ctx, teamrun.TeamRun{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot"}, members, events, completeness)
	stage := members[0].Stages[0]
	if stage.CurrentTaskID != "task-new" || len(stage.PublicUpdates) != 4 || stage.PublicUpdates[0].Text != "current draft" || stage.Status != "cancelled" || stage.OutputRefs[0] != "saved" {
		t.Fatalf("late event revived or replaced current attempt: %+v", stage)
	}
	for i, want := range []struct{ kind, taskID, text string }{{"text", "task-new", "current draft"}, {"text", "task-new", "late captured text"}, {"attempt", "task-old", ""}, {"attempt", "task-new", ""}} {
		update := stage.PublicUpdates[i]
		if update.Kind != want.kind || update.TaskID != want.taskID || (want.text != "" && update.Text != want.text) {
			t.Fatalf("wrong current text or physical attempt at %d: %+v", i, update)
		}
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM weave_team_runs WHERE run_id='run'`).Scan(&status); err != nil || status != "cancelled" {
		t.Fatalf("events changed terminal status: %s %v", status, err)
	}
	stored, err := tasks.Get(ctx, "ws", "task-new")
	if err != nil || string(stored.Result) != `{"output": "retained"}` {
		t.Fatalf("events changed final result: %v %v", stored, err)
	}
	// Both abandon causes share the same error code and clear cancellation
	// timestamps; only the actual transition can identify an unconfirmed stop.
	if _, err := pool.Exec(ctx, `UPDATE weave_team_runs SET status='abandoned',team_run_generation=1,error_code='team_run_execution_unrecoverable',updated_at=NOW(),terminal_at=NOW() WHERE run_id='run';
 INSERT INTO weave_team_run_transitions(workspace_id,run_id,seq,from_status,to_status,team_run_generation,execution_lease_epoch,resume_generation,actor,source,idempotency_key,error_code,occurred_at)
 VALUES('ws','run',1,'running','abandoned',1,0,0,'executor','worker','checkpoint-failed','team_run_execution_unrecoverable',NOW())`); err != nil {
		t.Fatal(err)
	}
	abandoned := teamrun.TeamRun{WorkspaceID: "ws", RunID: "run", Status: teamrun.StatusAbandoned, Generation: 1}
	if server.runActivityStopUnconfirmed(ctx, abandoned) {
		t.Fatal("checkpoint failure looked like an unconfirmed stop")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_run_transitions(workspace_id,run_id,seq,from_status,to_status,team_run_generation,execution_lease_epoch,resume_generation,actor,source,idempotency_key,error_code,occurred_at)
 VALUES('ws','run',2,'cancel_requested','abandoned',2,0,0,'sweeper','worker','stop-expired','team_run_execution_unrecoverable',NOW())`); err != nil {
		t.Fatal(err)
	}
	if server.runActivityStopUnconfirmed(ctx, abandoned) {
		t.Fatal("a different generation's stop changed this result")
	}
	abandoned.Generation = 2
	if !server.runActivityStopUnconfirmed(ctx, abandoned) {
		t.Fatal("recorded unconfirmed stop was hidden after cancellation timestamps were cleared")
	}
}
