package teamconstruction

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/agentcatalog"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamorch"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type buildExecutorFunc func(context.Context, string, string) (teamorch.Result, error)

func (f buildExecutorFunc) Execute(ctx context.Context, ws, id string) (teamorch.Result, error) {
	return f(ctx, ws, id)
}

type chatExecutorFunc func(context.Context, string, taskqueue.ChatExecRequest) (*taskqueue.ChatExecResult, error)

func (f chatExecutorFunc) ExecuteChat(ctx context.Context, ws string, req taskqueue.ChatExecRequest) (*taskqueue.ChatExecResult, error) {
	return f(ctx, ws, req)
}

func dispatchFixture(t *testing.T) (*Dispatcher, teambuild.TeamBuildRun) {
	t.Helper()
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "build-workspace", UserID: "user-1"})
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_workspaces (id,slug,name,created_at) VALUES ('build-workspace','build-workspace','Build tests',now())`); err != nil {
		t.Fatal(err)
	}
	runs := teambuild.New(pool, nil)
	now := time.Now().UTC()
	run, err := runs.CreateBuildRun(ctx, "build-workspace", "build-1", teambuild.CreateRunParams{
		Brief:     teambuild.BuildBrief{SchemaVersion: 1, Mode: teambuild.ModeCreate, BusinessDirection: "dispatch", Task: "verify dispatch", NewTeamName: "test-team", SuccessCriteria: []string{"one execution"}, AllowedAssets: teambuild.AssetScope{AllowedKinds: []string{"team"}, NamePrefix: "test-team"}, RoundBudget: teambuild.Budget{MaxInputTokens: 1}, TotalBudget: teambuild.Budget{MaxInputTokens: 1}},
		Contract:  teambuild.EvaluationContract{SchemaVersion: 1, HardGates: teambuild.DefaultFloorHardGates(), Rubric: []teambuild.RubricDimension{{ID: "dispatch", Name: "dispatch", Description: "dispatch", MaxScore: 1, PassThreshold: 1}}, PublicScenarios: []teambuild.Scenario{{ID: "dispatch", Input: "input", Expected: "output"}}, SevereDefectDefinition: "duplicate", RunCount: 1, MaxIterations: 3, PassRules: []string{"once"}, BlockRules: []string{"duplicate"}, InfraFailureRules: []string{"unavailable"}},
		ExpiresAt: now.Add(time.Hour), CreatedBy: "user-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Dispatch tests begin with an already-authorized business record. Authorization
	// and blueprint validity have their own contract tests.
	if _, err := pool.Exec(ctx, `UPDATE weave_team_build_runs SET status='authorized',confirmed_by='user-1' WHERE workspace_id=$1 AND build_run_id=$2`, run.WorkspaceID, run.BuildRunID); err != nil {
		t.Fatal(err)
	}
	run.Status = teambuild.StatusAuthorized
	tasks := taskqueue.New(pool, nil, 600*time.Millisecond)
	worker := taskqueue.NewWorker(tasks, 2)
	d := &Dispatcher{Pool: pool, Runs: runs, Tasks: tasks, Worker: worker, Executor: buildExecutorFunc(func(context.Context, string, string) (teamorch.Result, error) {
		return teamorch.Result{}, fmt.Errorf("executor not set by test")
	})}
	t.Cleanup(worker.Stop)
	return d, run
}

func waitDispatchTask(t *testing.T, d *Dispatcher, id string, accept func(*taskqueue.Task) bool) *taskqueue.Task {
	t.Helper()
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "build-workspace", UserID: "user-1"})
	deadline := time.Now().Add(8 * time.Second)
	var last *taskqueue.Task
	for time.Now().Before(deadline) {
		task, err := d.Tasks.Get(ctx, "build-workspace", id)
		if err != nil {
			t.Fatal(err)
		}
		last = task
		if accept(task) {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task did not reach expected state: %#v", last)
	return nil
}

func TestBuildDispatchUsesSinglePlatformTaskRealPG(t *testing.T) {
	d, run := dispatchFixture(t)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "build-workspace", UserID: "user-1"})
	const requests = 8
	var wg sync.WaitGroup
	results := make(chan ExecutionSubmission, requests)
	failures := make(chan error, requests)
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := d.Submit(ctx, run.WorkspaceID, run.BuildRunID)
			results <- r
			failures <- err
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := taskID(run.WorkspaceID, run.BuildRunID)
	for result := range results {
		if result.TaskID != id || result.Status != taskqueue.StatusQueued {
			t.Fatalf("submission: %#v", result)
		}
	}
	tasks, total, err := d.Tasks.List(ctx, run.WorkspaceID, 100, 0)
	if err != nil || total != 1 {
		t.Fatalf("tasks=%d err=%v", total, err)
	}
	if tasks[0].BuildRunID != run.BuildRunID || tasks[0].IdentityKind != taskqueue.IdentityTeamBuild || tasks[0].Source != TaskKind {
		t.Fatalf("identity=%#v", tasks[0])
	}
	if _, err := d.Submit(ctx, "another-workspace", run.BuildRunID); err == nil {
		t.Fatal("cross-workspace build submitted")
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE weave_task_queue SET build_run_id='different' WHERE id=$1`, id); err == nil {
		t.Fatal("build identity was mutable")
	}
	var retired *string
	if err := d.Pool.QueryRow(ctx, `SELECT to_regclass('weave_team_build_execution_jobs')::text`).Scan(&retired); err != nil || retired != nil {
		t.Fatalf("private queue retained: %v %v", retired, err)
	}
	d.Executor = buildExecutorFunc(func(ctx context.Context, ws, id string) (teamorch.Result, error) {
		current, ok := execution.CurrentTaskFromContext(ctx)
		if !ok || current.ID != taskID(ws, id) || current.WorkspaceID != ws || current.WorkerID == "" || current.ClaimEpoch < 1 {
			return teamorch.Result{}, fmt.Errorf("build lost current task claim")
		}
		for _, edge := range [][2]string{{teambuild.StatusAuthorized, teambuild.StatusRoundRunning}, {teambuild.StatusRoundRunning, teambuild.StatusPublishing}} {
			if _, err := d.Runs.TransitionStatus(ctx, ws, id, edge[0], edge[1], "test", "advance"); err != nil {
				return teamorch.Result{}, err
			}
		}
		_, err := d.Runs.MarkPublished(ctx, ws, id, "test", teambuild.FinalRef{Ref: "verified-publication"})
		return teamorch.Result{Status: teambuild.StatusPassed}, err
	})
	if err := d.Worker.Register(TaskKind, taskqueue.IdentityTeamBuild, d); err != nil {
		t.Fatal(err)
	}
	// Mechanical chat and TeamBuild share the same worker, with separate typed handlers.
	agent := registry.AgentRecord{Name: "mechanical", Role: "worker"}
	if err := agentcatalog.New(d.Pool).Put(ctx, run.WorkspaceID, &agent); err != nil {
		t.Fatal(err)
	}
	chatPayload, _ := json.Marshal(taskqueue.ChatExecRequest{Agent: "untrusted-payload-name", Message: "copy this"})
	chatTask := &taskqueue.Task{ID: "mechanical-task", WorkspaceID: run.WorkspaceID, Agent: agent.Name, AgentID: agent.ID, AgentVersion: agent.Version, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Source: "chat", Kind: "chat", Payload: chatPayload}
	if err := d.Tasks.Enqueue(ctx, chatTask); err != nil {
		t.Fatal(err)
	}
	if err := d.Worker.Register("chat", taskqueue.IdentityAgent, taskqueue.ChatHandler{Executor: chatExecutorFunc(func(ctx context.Context, ws string, req taskqueue.ChatExecRequest) (*taskqueue.ChatExecResult, error) {
		current, ok := execution.CurrentTaskFromContext(ctx)
		if !ok || current.ID != chatTask.ID || current.WorkspaceID != ws || current.WorkerID == "" || current.ClaimEpoch < 1 {
			return nil, fmt.Errorf("chat lost current task claim")
		}
		if ws != run.WorkspaceID || req.Agent != agent.Name || req.ExecutionStamp == nil || req.ExecutionStamp.AgentID != agent.ID {
			return nil, fmt.Errorf("chat lost durable identity")
		}
		return &taskqueue.ChatExecResult{Output: req.Message}, nil
	})}); err != nil {
		t.Fatal(err)
	}
	d.Worker.Start()
	chat := waitDispatchTask(t, d, chatTask.ID, func(task *taskqueue.Task) bool { return task.IsTerminal() })
	if chat.Status != taskqueue.StatusCompleted {
		t.Fatalf("mechanical task=%#v", chat)
	}
	var chatResult taskqueue.ChatExecResult
	if err := json.Unmarshal(chat.Result, &chatResult); err != nil || chatResult.Output != "copy this" {
		t.Fatalf("chat result=%#v %v", chatResult, err)
	}
	task := waitDispatchTask(t, d, id, func(task *taskqueue.Task) bool { return task.IsTerminal() })
	if task.Status != taskqueue.StatusCompleted || task.WorkerID != "" || task.ClaimEpoch != 1 {
		t.Fatalf("completed task=%#v", task)
	}
	var result teamorch.Result
	if err := json.Unmarshal(task.Result, &result); err != nil || result.Status != teambuild.StatusPassed {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestBuildCancellationWaitsForPhysicalReturnRealPG(t *testing.T) {
	d, run := dispatchFixture(t)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "build-workspace", UserID: "user-1"})
	started := make(chan struct{})
	cancelSeen := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	d.Executor = buildExecutorFunc(func(ctx context.Context, ws, id string) (teamorch.Result, error) {
		close(started)
		<-ctx.Done()
		close(cancelSeen)
		<-release
		return teamorch.Result{Status: teambuild.StatusPassed}, nil
	})
	if err := d.Worker.Register(TaskKind, taskqueue.IdentityTeamBuild, d); err != nil {
		t.Fatal(err)
	}
	submission, err := d.Submit(ctx, run.WorkspaceID, run.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	d.Worker.Start()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("not started")
	}
	cancelled, err := d.Cancel(ctx, run.WorkspaceID, run.BuildRunID, "user-1", "stop")
	if err != nil || cancelled.Status != teambuild.StatusCancelled {
		t.Fatalf("cancel=%#v err=%v", cancelled, err)
	}
	select {
	case <-cancelSeen:
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach handler")
	}
	pending, err := d.Tasks.Get(ctx, run.WorkspaceID, submission.TaskID)
	if err != nil || pending.Status != taskqueue.StatusCancelRequested || pending.WorkerID == "" {
		t.Fatalf("stop acknowledged prematurely: %#v %v", pending, err)
	}
	releaseOnce.Do(func() { close(release) })
	stopped := waitDispatchTask(t, d, submission.TaskID, func(task *taskqueue.Task) bool {
		return task.Status == taskqueue.StatusCancelled && task.WorkerID == ""
	})
	if len(stopped.Result) > 0 {
		t.Fatal("late success overwrote cancellation")
	}
}

func TestBuildCancellationAvailableWithoutExecutorRealPG(t *testing.T) {
	d, run := dispatchFixture(t)
	d.Executor = nil
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "build-workspace", UserID: "user-1"})
	if _, err := d.Submit(ctx, run.WorkspaceID, run.BuildRunID); err == nil {
		t.Fatal("missing execution service admitted a task")
	}
	cancelled, err := d.Cancel(ctx, run.WorkspaceID, run.BuildRunID, "user-1", "cancel unused build")
	if err != nil || cancelled.Status != teambuild.StatusCancelled {
		t.Fatalf("business cancellation depends on optional executor: %#v %v", cancelled, err)
	}
	_, total, err := d.Tasks.List(ctx, run.WorkspaceID, 10, 0)
	if err != nil || total != 0 {
		t.Fatalf("unavailable executor left queued work: %d %v", total, err)
	}
}

func TestBuildLeaseLossRequiresStopBeforeExplicitResumeRealPG(t *testing.T) {
	d, run := dispatchFixture(t)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "build-workspace", UserID: "user-1"})
	submission, err := d.Submit(ctx, run.WorkspaceID, run.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	first, err := d.Tasks.Claim(ctx, "missing-worker", taskqueue.ClaimFilter{Kind: TaskKind, IdentityKind: taskqueue.IdentityTeamBuild})
	if err != nil || first == nil {
		t.Fatalf("claim=%#v %v", first, err)
	}
	originalDeadline := *first.DeadlineAt
	firstUsage := &execution.TerminalUsage{InputTokens: 11, OutputTokens: 3, CostUSD: 0.12, ToolCalls: 1}
	if err := d.Tasks.RecordClaimUsage(ctx, first.ID, first.WorkerID, first.ClaimEpoch, firstUsage); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `UPDATE weave_task_queue SET lease_expires_at=now()-interval '1 minute' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.Tasks.Heartbeat(ctx, first.ID, first.WorkerID); err == nil {
		t.Fatal("expired owner renewed")
	}
	if _, err := d.Tasks.RecoverStale(ctx); err != nil {
		t.Fatal(err)
	}
	unknown, err := d.Tasks.Get(ctx, run.WorkspaceID, first.ID)
	if err != nil || unknown.Status != taskqueue.StatusFailed || unknown.WorkerID != first.WorkerID {
		t.Fatalf("lost owner=%#v %v", unknown, err)
	}
	if _, err := d.Submit(ctx, run.WorkspaceID, run.BuildRunID); err == nil {
		t.Fatal("unconfirmed execution was resumed")
	}
	if next, err := d.Tasks.Claim(ctx, "second-worker", taskqueue.ClaimFilter{Kind: TaskKind, IdentityKind: taskqueue.IdentityTeamBuild}); err != nil || next != nil {
		t.Fatalf("unknown execution automatically claimed: %#v %v", next, err)
	}
	if err := d.Tasks.AcknowledgeExecutionStopped(ctx, first.ID, first.WorkerID); err != nil {
		t.Fatal(err)
	}
	resumed, err := d.Submit(ctx, run.WorkspaceID, run.BuildRunID)
	if err != nil || resumed.TaskID != submission.TaskID || resumed.Status != taskqueue.StatusQueued {
		t.Fatalf("resume=%#v %v", resumed, err)
	}
	next, err := d.Tasks.Claim(ctx, "second-worker", taskqueue.ClaimFilter{Kind: TaskKind, IdentityKind: taskqueue.IdentityTeamBuild})
	if err != nil || next == nil || next.ClaimEpoch != 2 {
		t.Fatalf("resumed claim=%#v %v", next, err)
	}
	if next.DeadlineAt == nil || !next.DeadlineAt.Equal(originalDeadline) {
		t.Fatalf("resume extended absolute deadline: first=%s next=%v", originalDeadline, next.DeadlineAt)
	}
	secondUsage := &execution.TerminalUsage{InputTokens: 7, OutputTokens: 2, CostUSD: 0.08, ToolCalls: 2}
	if err := d.Tasks.RecordClaimUsage(ctx, next.ID, next.WorkerID, next.ClaimEpoch, secondUsage); err != nil {
		t.Fatal(err)
	}
	if err := d.Tasks.RecordClaimUsage(ctx, next.ID, next.WorkerID, next.ClaimEpoch, secondUsage); err != nil {
		t.Fatalf("usage replay was not idempotent: %v", err)
	}
	if err := d.Tasks.CompleteClaimed(ctx, first.ID, first.WorkerID, json.RawMessage(`{"late":true}`), ""); err == nil {
		t.Fatal("old execution overwrote new attempt")
	}
	if err := d.Tasks.CompleteClaimed(ctx, next.ID, next.WorkerID, json.RawMessage(`{"resumed":true}`), ""); err != nil {
		t.Fatal(err)
	}
	completed, err := d.Tasks.Get(ctx, run.WorkspaceID, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.PhysicalUsage.InputTokens != 18 || completed.PhysicalUsage.OutputTokens != 5 ||
		completed.PhysicalUsage.ToolCalls != 3 || completed.PhysicalUsage.CostUSD != 0.2 || completed.UnreportedAttempts != 0 {
		t.Fatalf("physical usage changed across resume: %#v unreported=%d", completed.PhysicalUsage, completed.UnreportedAttempts)
	}
}

func TestBuildContinuationUsesPlatformAvailabilityRealPG(t *testing.T) {
	d, run := dispatchFixture(t)
	ctx := execution.WithSubject(context.Background(), execution.Subject{WorkspaceID: "build-workspace", UserID: "user-1"})
	d.Executor = buildExecutorFunc(func(context.Context, string, string) (teamorch.Result, error) {
		return teamorch.Result{Status: teambuild.StatusRoundRunning, StopReason: "waiting for existing operation"}, nil
	})
	if err := d.Worker.Register(TaskKind, taskqueue.IdentityTeamBuild, d); err != nil {
		t.Fatal(err)
	}
	submission, err := d.Submit(ctx, run.WorkspaceID, run.BuildRunID)
	if err != nil {
		t.Fatal(err)
	}
	d.Worker.Start()
	queued := waitDispatchTask(t, d, submission.TaskID, func(task *taskqueue.Task) bool { return task.Status == taskqueue.StatusQueued && task.ClaimEpoch == 1 })
	if !queued.AvailableAt.After(time.Now().Add(20 * time.Second)) {
		t.Fatalf("continuation backoff missing: %s", queued.AvailableAt)
	}
	if task, err := d.Tasks.Claim(ctx, "other-worker", taskqueue.ClaimFilter{Kind: TaskKind, IdentityKind: taskqueue.IdentityTeamBuild}); err != nil || task != nil {
		t.Fatal(fmt.Sprintf("premature continuation: %#v %v", task, err))
	}
}
