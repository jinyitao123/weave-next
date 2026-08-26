package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestExecutorProcessNextQueuedRunSucceeds(t *testing.T) {
	h := newProcessNextHarness(t)
	h.runtime.executeResult = RuntimeResult{
		Status: RuntimeCompleted,
		Output: json.RawMessage(`{"ok":true}`),
		Usage:  UsageTotals{InputTokens: 3, OutputTokens: 5, CostUSD: 0.25},
	}
	taskID := h.enqueueWorkflowTask(t, "run-success")

	processed, err := h.executor.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	h.assertTask(t, taskID, taskqueue.StatusCompleted, "run-success", "")
	h.assertRun(t, "run-success", StatusSucceeded, nil)
	h.assertTerminalMarker(t, "run-success", "success", "completed")
}

func TestExecutorProcessNextRuntimeErrorFailsRunAndTask(t *testing.T) {
	h := newProcessNextHarness(t)
	h.runtime.executeResult = RuntimeResult{
		Status: RuntimeFailed,
		Usage:  UsageTotals{InputTokens: 1},
	}
	h.runtime.executeErr = executionError(ErrorCodeOutputInvalid, errors.New("bad workflow output"))
	taskID := h.enqueueWorkflowTask(t, "run-failed")

	processed, err := h.executor.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	h.assertTask(t, taskID, taskqueue.StatusFailed, "", string(ErrorCodeOutputInvalid))
	h.assertRun(t, "run-failed", StatusFailed, &[]ErrorCode{ErrorCodeOutputInvalid}[0])
	h.assertTerminalMarker(t, "run-failed", "failed", string(ErrorCodeOutputInvalid))
}

func TestExecutorProcessNextParkTimerCompletesTaskAndPersistsCheckpoint(t *testing.T) {
	h := newProcessNextHarness(t)
	h.runtime.executeResult = RuntimeResult{
		Status: RuntimeParked,
		Park: &RuntimePark{
			NodeID:           "wait-node",
			CompletedOutputs: map[string]json.RawMessage{"lead": json.RawMessage(`{"done":true}`)},
			WaitKind:         WaitTimer,
			WaitDetail:       json.RawMessage(`{"wake_at":"2026-08-25T10:05:00Z","node_id":"wait-node"}`),
			UsageCheckpoint:  json.RawMessage(`{"schema_version":1,"next_call_ordinals":{},"calls":[]}`),
			UsageComplete:    true,
		},
	}
	taskID := h.enqueueWorkflowTask(t, "run-park")

	processed, err := h.executor.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	h.assertTask(t, taskID, taskqueue.StatusCompleted, "run-park", "")
	h.assertRun(t, "run-park", StatusParked, nil)
	checkpoint, err := h.executor.Checkpoints.GetTx(context.Background(), h.mustBeginTx(t), "workspace-1", "run-park")
	if err != nil {
		t.Fatalf("read checkpoint: %v", err)
	}
	if checkpoint.NodeID != "wait-node" || checkpoint.CompletedOutputs["lead"] == nil {
		t.Fatalf("unexpected checkpoint: %#v", checkpoint)
	}
}

func TestExecutorProcessNextRunningRunReclaimsLeaseAndExecutesFromStart(t *testing.T) {
	h := newProcessNextHarness(t)
	h.runtime.executeResult = RuntimeResult{
		Status: RuntimeCompleted,
		Output: json.RawMessage(`{"reclaimed":true}`),
		Usage:  UsageTotals{InputTokens: 7, OutputTokens: 11, CostUSD: 0.5},
	}
	taskID, running := h.seedRunningWorkflowTaskBeforeAdmission(t, "run-reclaim")

	processed, err := h.executor.ProcessNext(context.Background(), "worker-2")
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	if h.runtime.executeCalls != 1 {
		t.Fatalf("execute calls = %d, want 1", h.runtime.executeCalls)
	}
	if h.runtime.resumeCalls != 0 {
		t.Fatalf("resume calls = %d, want 0", h.runtime.resumeCalls)
	}
	h.assertTask(t, taskID, taskqueue.StatusCompleted, "run-reclaim", "")
	h.assertRun(t, "run-reclaim", StatusSucceeded, nil)
	h.assertTerminalMarker(t, "run-reclaim", "success", "completed")
	terminal := h.readRun(t, "run-reclaim")
	if terminal.ExecutionLeaseEpoch != running.ExecutionLeaseEpoch+1 {
		t.Fatalf("terminal execution lease epoch = %d, want reclaimed epoch %d", terminal.ExecutionLeaseEpoch, running.ExecutionLeaseEpoch+1)
	}
}

func TestExecutorProcessNextReplaysSuccessTerminalMarkerWithoutRuntime(t *testing.T) {
	h := newProcessNextHarness(t)
	taskID, running := h.seedRunningWorkflowTask(t, "run-replay-success")
	if err := h.executor.commitFrozenNormalTerminal(
		context.Background(),
		running,
		"success",
		"completed",
		UsageTotals{InputTokens: 2, OutputTokens: 3},
		true,
		"",
	); err != nil {
		t.Fatalf("commit terminal marker: %v", err)
	}
	h.runtime.executeErr = errors.New("runtime must not run during terminal replay")
	h.runtime.resumeErr = errors.New("runtime must not resume during terminal replay")

	processed, err := h.executor.ProcessNext(context.Background(), "worker-2")
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	if h.runtime.executeCalls != 0 || h.runtime.resumeCalls != 0 {
		t.Fatalf("runtime calls = execute:%d resume:%d, want none", h.runtime.executeCalls, h.runtime.resumeCalls)
	}
	h.assertTask(t, taskID, taskqueue.StatusCompleted, "run-replay-success", "")
	h.assertRun(t, "run-replay-success", StatusSucceeded, nil)
	h.assertTerminalMarker(t, "run-replay-success", "success", "completed")
}

func TestExecutorProcessNextReplaysFailedTerminalMarkerWithoutRuntime(t *testing.T) {
	h := newProcessNextHarness(t)
	taskID, running := h.seedRunningWorkflowTask(t, "run-replay-failed")
	if err := h.executor.commitFrozenNormalTerminal(
		context.Background(),
		running,
		"failed",
		string(ErrorCodeOutputInvalid),
		UsageTotals{InputTokens: 2},
		true,
		"",
	); err != nil {
		t.Fatalf("commit terminal marker: %v", err)
	}
	h.runtime.executeErr = errors.New("runtime must not run during terminal replay")
	h.runtime.resumeErr = errors.New("runtime must not resume during terminal replay")

	processed, err := h.executor.ProcessNext(context.Background(), "worker-2")
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	if h.runtime.executeCalls != 0 || h.runtime.resumeCalls != 0 {
		t.Fatalf("runtime calls = execute:%d resume:%d, want none", h.runtime.executeCalls, h.runtime.resumeCalls)
	}
	h.assertTask(t, taskID, taskqueue.StatusFailed, "", string(ErrorCodeOutputInvalid))
	h.assertRun(t, "run-replay-failed", StatusFailed, &[]ErrorCode{ErrorCodeOutputInvalid}[0])
	h.assertTerminalMarker(t, "run-replay-failed", "failed", string(ErrorCodeOutputInvalid))
}

func TestExecutorProcessNextFanoutLegRecordsTerminal(t *testing.T) {
	task := &taskqueue.Task{
		ID: "fanout-task", WorkspaceID: "workspace-1", ContextKey: "group-1",
		Kind: "team_workflow", WorkerID: "worker-1",
		Payload: json.RawMessage(`{
			"schema_version":1,
			"kind":"fanout_leg",
			"workspace_id":"workspace-1",
			"parent_run_id":"parent-run",
			"intent_id":"intent-1",
			"group_id":"group-1",
			"leg_id":"leg-1",
			"branch_id":"branch-1",
			"branch_ordinal":0,
			"generation":"gen-1",
			"frozen_bundle_ref":{},
			"input_ref":{},
			"may_yield_proof":{}
		}`),
	}
	tasks := &fakeProcessNextTasks{claimed: task}
	runtime := &scriptedRuntime{fanoutOutput: json.RawMessage(`{"leg":"ok"}`)}
	fanout := &fakeFanout{}
	executor := fakeFanoutExecutor(tasks, runtime, fanout)

	processed, err := executor.ProcessNext(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("process next: %v", err)
	}
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	if tasks.completedRunID != "parent-run" || string(tasks.completedResult) != `{"leg":"ok"}` {
		t.Fatalf("unexpected completed task: run=%q result=%s", tasks.completedRunID, tasks.completedResult)
	}
	if fanout.completion.Terminal != "succeeded" || fanout.completion.LegID != "leg-1" {
		t.Fatalf("unexpected fanout completion: %#v", fanout.completion)
	}
}

func TestExecutorProcessNextLeaseLostReturnsWithoutFailingTask(t *testing.T) {
	task := &taskqueue.Task{
		ID: "fanout-task", WorkspaceID: "workspace-1", ContextKey: "group-1",
		Kind: "team_workflow", WorkerID: "worker-1",
		Payload: json.RawMessage(`{
			"schema_version":1,
			"kind":"fanout_leg",
			"workspace_id":"workspace-1",
			"parent_run_id":"parent-run",
			"intent_id":"intent-1",
			"group_id":"group-1",
			"leg_id":"leg-1",
			"branch_id":"branch-1",
			"branch_ordinal":0,
			"generation":"gen-1",
			"frozen_bundle_ref":{},
			"input_ref":{},
			"may_yield_proof":{}
		}`),
	}
	tasks := &fakeProcessNextTasks{claimed: task, heartbeatErr: errors.New("lost")}
	runtime := &scriptedRuntime{fanoutBlock: 50 * time.Millisecond}
	fanout := &fakeFanout{}
	executor := fakeFanoutExecutor(tasks, runtime, fanout)
	executor.HeartbeatInterval = time.Millisecond

	processed, err := executor.ProcessNext(context.Background(), "worker-1")
	if !processed {
		t.Fatalf("processed = false, want true")
	}
	if !errors.Is(err, errTaskLeaseLost) {
		t.Fatalf("err = %v, want errTaskLeaseLost", err)
	}
	if tasks.completed || tasks.failed {
		t.Fatalf("lease-lost task should not be completed or failed")
	}
	if fanout.completion.Terminal != "" {
		t.Fatalf("lease-lost fanout should not record terminal: %#v", fanout.completion)
	}
}

type processNextHarness struct {
	pool            *pgxpool.Pool
	tasks           *taskqueue.Store
	runtime         *scriptedRuntime
	executor        *Executor
	now             time.Time
	publishedSeeded bool
}

func newProcessNextHarness(t *testing.T) *processNextHarness {
	t.Helper()
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	now := time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)
	tasks := taskqueue.New(pool, fixedTaskClock{now: now}, time.Minute)
	runtime := &scriptedRuntime{}
	snapshots := snapshot.NewStore(pool)
	executor := &Executor{
		Tasks: tasks,
		Consumer: &Consumer{
			Transactions: pool,
			Snapshots:    snapshots,
			Runs:         NewPGStore(),
			Tasks:        tasks,
			Now:          func() time.Time { return now },
		},
		Transactions:      pool,
		Runs:              NewPGStore(),
		Checkpoints:       NewPGCheckpointStore(),
		Runtime:           runtime,
		Now:               func() time.Time { return now },
		ResumeTokenHash:   func() ([]byte, error) { return []byte("timer-resume-token-hash"), nil },
		HeartbeatInterval: time.Second,
	}
	return &processNextHarness{
		pool: pool, tasks: tasks, runtime: runtime, executor: executor, now: now,
	}
}

func (h *processNextHarness) enqueueWorkflowTask(t *testing.T, runID string) string {
	t.Helper()
	ctx := context.Background()
	h.seedWorkflowSnapshot(t, runID)
	taskID := "task-" + runID
	err := h.tasks.Enqueue(ctx, &taskqueue.Task{
		ID: taskID, WorkspaceID: "workspace-1",
		IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: "workflow-1", WorkflowVersion: 1, RunSnapshotID: runID,
		Source: "api", Kind: "team_workflow", Payload: json.RawMessage(`{"input":true}`),
	})
	if err != nil {
		t.Fatalf("enqueue workflow task: %v", err)
	}
	return taskID
}

func (h *processNextHarness) seedRunningWorkflowTask(t *testing.T, runID string) (string, TeamRun) {
	t.Helper()
	ctx := context.Background()
	taskID := h.enqueueWorkflowTask(t, runID)
	claimed, err := h.tasks.Claim(ctx, "seed-worker", taskqueue.ClaimFilter{
		Kind:          "team_workflow",
		IdentityKind:  taskqueue.IdentityTeamWorkflow,
		WorkspaceID:   "workspace-1",
		RunSnapshotID: runID,
	})
	if err != nil {
		t.Fatalf("claim seed task: %v", err)
	}
	if claimed == nil {
		t.Fatalf("claim seed task returned nil")
	}
	queued, err := h.executor.Consumer.ConsumeClaimed(ctx, claimed, "seed-worker")
	if err != nil {
		t.Fatalf("consume seed task: %v", err)
	}
	running, err := h.executor.claimRunning(ctx, queued, claimed, executorIdentity(claimed.ID, "seed-worker"))
	if err != nil {
		t.Fatalf("claim seed run running: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status='queued', worker_id=NULL, started_at=NULL, lease_expires_at=NULL, updated_at=$2
		WHERE id=$1
	`, taskID, h.now); err != nil {
		t.Fatalf("restore task for reclaim: %v", err)
	}
	return taskID, running
}

func (h *processNextHarness) seedRunningWorkflowTaskBeforeAdmission(t *testing.T, runID string) (string, TeamRun) {
	t.Helper()
	ctx := context.Background()
	taskID := h.enqueueWorkflowTask(t, runID)
	claimed, err := h.tasks.Claim(ctx, "seed-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", IdentityKind: taskqueue.IdentityTeamWorkflow,
		WorkspaceID: "workspace-1", RunSnapshotID: runID,
	})
	if err != nil {
		t.Fatalf("claim seed task before admission: %v", err)
	}
	if claimed == nil {
		t.Fatal("claim seed task before admission returned nil")
	}
	queued, err := h.executor.Consumer.ConsumeClaimed(ctx, claimed, "seed-worker")
	if err != nil {
		t.Fatalf("consume seed task before admission: %v", err)
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed running before admission: %v", err)
	}
	running, err := NewPGStore().ClaimRunningTx(ctx, tx, ClaimRequest{
		WorkspaceID: queued.WorkspaceID, RunID: queued.RunID,
		ExpectedStatus: StatusQueued, ExpectedTeamRunGeneration: queued.Generation,
		ExpectedExecutionLeaseEpoch: queued.ExecutionLeaseEpoch,
		ExpectedResumeGeneration:    queued.ResumeGeneration,
		ExecutorID:                  executorIdentity(claimed.ID, "seed-worker"),
		IdempotencyKey:              executorClaimKey(claimed.ID),
		Actor:                       executorIdentity(claimed.ID, "seed-worker"), Source: consumerSource, OccurredAt: h.now,
	})
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed running before admission: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed running before admission: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		UPDATE weave_task_queue
		SET status='queued', worker_id=NULL, started_at=NULL, lease_expires_at=NULL, updated_at=$2
		WHERE id=$1
	`, taskID, h.now); err != nil {
		t.Fatalf("restore task for pre-admission reclaim: %v", err)
	}
	return taskID, running
}

func (h *processNextHarness) seedWorkflowSnapshot(t *testing.T, runID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO weave_workspaces (id, slug, name, created_at)
		VALUES ('workspace-1','workspace-1','workspace-1',$1)
		ON CONFLICT DO NOTHING;
		INSERT INTO weave_teams (id, workspace_id, name, created_at)
		VALUES ('team-1','workspace-1','Team 1',$1)
		ON CONFLICT DO NOTHING;
	`, h.now); err != nil {
		t.Fatalf("seed workspace/team: %v", err)
	}
	contentHash := h.seedPublishedWorkflowArtifact(t)
	buildRunID := "build-" + runID
	triggerSource, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
	}{SchemaVersion: 1, Type: "api", SourceRef: buildRunID})
	if err != nil {
		t.Fatalf("marshal API trigger source: %v", err)
	}
	_, err = snapshot.NewStore(h.pool).Create(ctx, snapshot.TeamRunSnapshot{
		RunID: runID, WorkspaceID: "workspace-1", TeamID: "team-1",
		SnapshotSchemaVersion: 2, Mode: "fixed_workflow",
		WorkflowID: "workflow-1", WorkflowVersion: 1,
		ArtifactWorkflowID: "workflow-1", ArtifactWorkflowVersion: 1,
		AdmissionDecision: json.RawMessage(`{
			"schema_version":1,
			"team_active":true,
			"workflow_active":true,
			"workers_enabled":true,
			"version_blocked":false,
			"decided_at":"2026-08-25T10:00:00Z"
		}`),
		RunAssociations: json.RawMessage(`{
			"schema_version":1,
			"parent_run_id":null,
			"source_snapshot_id":null,
			"task_group_id":null
		}`),
		TriggerSourceV2:      triggerSource,
		RuntimeAssignment:    json.RawMessage(`{}`),
		BuildRunID:           buildRunID,
		CandidateContentHash: contentHash,
	})
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
}

func (h *processNextHarness) seedPublishedWorkflowArtifact(t *testing.T) string {
	t.Helper()
	if h.publishedSeeded {
		var contentHash string
		if err := h.pool.QueryRow(context.Background(), `
			SELECT content_hash
			FROM weave_published_artifact_contents
			WHERE workspace_id='workspace-1' AND workflow_id='workflow-1' AND workflow_version=1
		`).Scan(&contentHash); err != nil {
			t.Fatalf("read published workflow artifact hash: %v", err)
		}
		return contentHash
	}
	ctx := context.Background()
	trigger := json.RawMessage(`{"schema_version":1}`)
	graph := json.RawMessage(`{"schema_version":1}`)
	payload := frozen.ArtifactPayloadV1{
		SchemaVersion: frozen.ArtifactSchemaVersion, TriggerConfig: trigger, GraphDefinition: graph,
		Team:    frozen.ArtifactTeamV1{WorkspaceID: "workspace-1", TeamID: "team-1", LeadAgentID: "fixture-lead"},
		Bundles: []frozen.FrozenExecutionBundle{}, DeliveryTargets: []frozen.FrozenDeliveryTarget{},
	}
	contentHash, err := frozen.ComputeArtifactContentHash(frozen.ArtifactEnvelopeHashInputV1{
		WorkspaceID: "workspace-1", WorkflowID: "workflow-1", WorkflowVersion: 1,
		ArtifactSchemaVersion:     frozen.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: frozen.ArtifactCanonicalizationAlgorithm,
		CanonicalizationVersion:   frozen.ArtifactCanonicalizationVersion,
		HashAlgorithm:             frozen.ArtifactHashAlgorithm, Payload: payload,
	})
	if err != nil {
		t.Fatalf("hash published workflow fixture: %v", err)
	}
	payloadJSON, err := frozen.Canonicalize(payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		t.Fatalf("canonicalize published workflow fixture: %v", err)
	}
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO weave_team_workflows (workspace_id,id,team_id,name)
		VALUES ('workspace-1','workflow-1','team-1','Workflow 1');
		INSERT INTO weave_team_workflow_versions (
			workspace_id,workflow_id,version,status,trigger_config,graph_definition,created_by
		) VALUES ('workspace-1','workflow-1',1,'draft',$1::jsonb,$2::jsonb,'fixture');
		UPDATE weave_team_workflow_versions
		SET status='published',published_at=$3,updated_at=$3
		WHERE workspace_id='workspace-1' AND workflow_id='workflow-1' AND version=1;
		INSERT INTO weave_published_artifact_contents (
			workspace_id,workflow_id,workflow_version,artifact_schema_version,
			canonicalization_algorithm,canonicalization_version,hash_algorithm,content_hash,payload
		) VALUES ('workspace-1','workflow-1',1,1,'rfc8785+jcs-preorder',1,'sha256',$4,$5::jsonb);
		INSERT INTO weave_workflow_version_admission_statuses (workspace_id,workflow_id,workflow_version,blocked)
		VALUES ('workspace-1','workflow-1',1,false);
		UPDATE weave_team_workflows SET published_version=1,updated_at=$3
		WHERE workspace_id='workspace-1' AND id='workflow-1'
	`, string(trigger), string(graph), h.now, contentHash, string(payloadJSON)); err != nil {
		t.Fatalf("seed published workflow artifact: %v", err)
	}
	h.publishedSeeded = true
	return contentHash
}

func (h *processNextHarness) assertTask(t *testing.T, taskID, status, runID, errContains string) {
	t.Helper()
	task, err := h.tasks.Get(context.Background(), "workspace-1", taskID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.Status != status {
		t.Fatalf("task status = %q, want %q; task=%#v", task.Status, status, task)
	}
	if runID != "" && task.RunID != runID {
		t.Fatalf("task run_id = %q, want %q", task.RunID, runID)
	}
	if errContains != "" && !strings.Contains(task.Error, errContains) {
		t.Fatalf("task error = %q, want containing %q", task.Error, errContains)
	}
}

func (h *processNextHarness) readRun(t *testing.T, runID string) TeamRun {
	t.Helper()
	tx := h.mustBeginTx(t)
	run, err := h.executor.Runs.GetForUpdateTx(context.Background(), tx, "workspace-1", runID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	return run
}

func (h *processNextHarness) assertRun(t *testing.T, runID string, status Status, code *ErrorCode) {
	t.Helper()
	var statusRaw string
	var errorCode *string
	err := h.pool.QueryRow(context.Background(), `
		SELECT status,error_code
		FROM weave_team_runs
		WHERE workspace_id='workspace-1' AND run_id=$1
	`, runID).Scan(&statusRaw, &errorCode)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if Status(statusRaw) != status {
		t.Fatalf("run status = %q, want %q", statusRaw, status)
	}
	if code == nil {
		if errorCode != nil {
			t.Fatalf("run error_code = %q, want nil", *errorCode)
		}
		return
	}
	if errorCode == nil || *errorCode != string(*code) {
		t.Fatalf("run error_code = %v, want %q", errorCode, *code)
	}
}

func (h *processNextHarness) assertTerminalMarker(t *testing.T, runID, status, stopReason string) {
	t.Helper()
	var statusRaw, stopReasonRaw string
	err := h.pool.QueryRow(context.Background(), `
		SELECT status,stop_reason
		FROM weave_run_terminal_markers
		WHERE workspace_id='workspace-1' AND run_id=$1
	`, runID).Scan(&statusRaw, &stopReasonRaw)
	if err != nil {
		t.Fatalf("read terminal marker: %v", err)
	}
	if statusRaw != status || stopReasonRaw != stopReason {
		t.Fatalf("terminal marker = (%q,%q), want (%q,%q)", statusRaw, stopReasonRaw, status, stopReason)
	}
}

func (h *processNextHarness) mustBeginTx(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := h.pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

type fixedTaskClock struct {
	now time.Time
}

func (c fixedTaskClock) Now() time.Time { return c.now }

type scriptedRuntime struct {
	executeResult RuntimeResult
	executeErr    error
	resumeResult  RuntimeResult
	resumeErr     error
	fanoutOutput  json.RawMessage
	fanoutErr     error
	fanoutBlock   time.Duration
	executeCalls  int
	resumeCalls   int
}

func (r *scriptedRuntime) Execute(context.Context, TeamRun, *taskqueue.Task) (RuntimeResult, error) {
	r.executeCalls++
	return r.executeResult, r.executeErr
}

func (r *scriptedRuntime) ResumeCheckpoint(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (RuntimeResult, error) {
	r.resumeCalls++
	return r.resumeResult, r.resumeErr
}

func (r *scriptedRuntime) TimerResumeTarget(context.Context, TeamRun, *taskqueue.Task, WorkflowCheckpointV1) (string, bool, error) {
	return "", false, nil
}

func (r *scriptedRuntime) ExecuteFanoutLeg(ctx context.Context, _ *taskqueue.Task, _ FanoutLegTaskPayloadV1) (json.RawMessage, error) {
	if r.fanoutBlock > 0 {
		select {
		case <-time.After(r.fanoutBlock):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.fanoutOutput, r.fanoutErr
}

type fakeProcessNextTasks struct {
	claimed          *taskqueue.Task
	heartbeatErr     error
	completed        bool
	completedResult  json.RawMessage
	completedRunID   string
	failed           bool
	failedErrMessage string
}

func (s *fakeProcessNextTasks) Claim(context.Context, string, taskqueue.ClaimFilter) (*taskqueue.Task, error) {
	return s.claimed, nil
}

func (s *fakeProcessNextTasks) Get(context.Context, string, string) (*taskqueue.Task, error) {
	return s.claimed, nil
}

func (s *fakeProcessNextTasks) Heartbeat(context.Context, string, string) error {
	return s.heartbeatErr
}

func (s *fakeProcessNextTasks) CompleteClaimed(_ context.Context, _ string, _ string, result json.RawMessage, runID string) error {
	s.completed = true
	s.completedResult = append(json.RawMessage(nil), result...)
	s.completedRunID = runID
	return nil
}

func (s *fakeProcessNextTasks) FailClaimed(_ context.Context, _ string, _ string, message string) error {
	s.failed = true
	s.failedErrMessage = message
	return nil
}

type fakeFanout struct {
	completion FanoutLegCompletion
}

func (f *fakeFanout) PreparePark(context.Context, pgx.Tx, FanoutPrepareRequest) (FanoutParkIntent, error) {
	return FanoutParkIntent{}, nil
}

func (f *fakeFanout) ActivatePark(context.Context, FanoutActivationRequest) error {
	return nil
}

func (f *fakeFanout) RecordLegCompletion(_ context.Context, completion FanoutLegCompletion) error {
	f.completion = completion
	return nil
}

func fakeFanoutExecutor(tasks *fakeProcessNextTasks, runtime *scriptedRuntime, fanout *fakeFanout) *Executor {
	return &Executor{
		Tasks:        tasks,
		Consumer:     &Consumer{},
		Transactions: &fakeTransactionBeginner{},
		Runs:         &fakeRunStore{},
		Checkpoints:  &fakeCheckpointStore{},
		Runtime:      runtime,
		Fanout:       fanout,
		Now:          func() time.Time { return time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC) },
	}
}

type fakeTransactionBeginner struct{}

func (*fakeTransactionBeginner) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unused")
}

type fakeRunStore struct{}

func (*fakeRunStore) GetForUpdateTx(context.Context, pgx.Tx, string, string) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) ClaimRunningTx(context.Context, pgx.Tx, ClaimRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) ParkTx(context.Context, pgx.Tx, ParkRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) ReclaimRunningTx(context.Context, pgx.Tx, ReclaimRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) AbandonUnrecoverableTx(context.Context, pgx.Tx, AbandonUnrecoverableRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) ResumeRunningTx(context.Context, pgx.Tx, ResumeRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) FailWaitTimeoutTx(context.Context, pgx.Tx, FailWaitTimeoutRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) SucceedTx(context.Context, pgx.Tx, SucceedRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}
func (*fakeRunStore) FailTx(context.Context, pgx.Tx, FailRequest) (TeamRun, error) {
	return TeamRun{}, errors.New("unused")
}

type fakeCheckpointStore struct{}

func (*fakeCheckpointStore) PutTx(context.Context, pgx.Tx, WorkflowCheckpointV1) (string, error) {
	return "", errors.New("unused")
}

func (*fakeCheckpointStore) GetTx(context.Context, pgx.Tx, string, string) (WorkflowCheckpointV1, error) {
	return WorkflowCheckpointV1{}, errors.New("unused")
}
