package teambuild

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestCompilerProgressUsesPlatformTaskFenceRealPG(t *testing.T) {
	subject := execution.Subject{WorkspaceID: "workspace-1", UserID: "admin-1"}
	ctx := execution.WithSubject(context.Background(), subject)
	store := newAuthorizeTestStore(t)
	run := authorizeBudgetTestRun(t, ctx, store, createAuthorizeTestRun(t, ctx, store, "compiler-platform-fence", ModeCreate))

	tasks := taskqueue.New(store.pool, nil, time.Minute)
	deadline := time.Now().UTC().Add(time.Hour)
	task := &taskqueue.Task{
		ID: "compiler-platform-fence-task", WorkspaceID: run.WorkspaceID,
		BuildRunID: run.BuildRunID, Kind: "team_build", Source: "team_build",
		IdentityKind: taskqueue.IdentityTeamBuild, IdentitySchemaVersion: 2,
		DeadlineAt: &deadline, OutcomeSensitive: true, Payload: json.RawMessage(`{"build_run_id":"compiler-platform-fence"}`),
	}
	if err := tasks.Enqueue(ctx, task); err != nil {
		t.Fatal(err)
	}
	claimed, err := tasks.Claim(ctx, "compiler-worker-1", taskqueue.ClaimFilter{Kind: "team_build", IdentityKind: taskqueue.IdentityTeamBuild})
	if err != nil || claimed == nil {
		t.Fatalf("claim platform task: %#v %v", claimed, err)
	}
	bound, err := taskqueue.BindTaskExecution(ctx, claimed, tasks)
	if err != nil {
		t.Fatal(err)
	}

	first, err := store.NextReadyOperationStep(bound, run.WorkspaceID, run.BuildRunID, 1)
	if err != nil {
		t.Fatal(err)
	}
	const outputHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, finishErr := store.FinishOperationStep(bound, first.WorkspaceID, first.BuildRunID, first.RevisionNo,
				first.OperationID, OperationStatusSucceeded, outputHash, "", "", json.RawMessage(`{"compiled":true}`), tasks.ValidateCurrentTaskTx)
			errs <- finishErr
		}()
	}
	wg.Wait()
	close(errs)
	var succeeded, fenced int
	for finishErr := range errs {
		switch {
		case finishErr == nil:
			succeeded++
		case errors.Is(finishErr, ErrOperationStepConflict):
			fenced++
		default:
			t.Fatalf("unexpected concurrent finish error: %v", finishErr)
		}
	}
	if succeeded != 1 || fenced != 1 {
		t.Fatalf("concurrent finish success=%d conflict=%d", succeeded, fenced)
	}

	second, err := store.NextReadyOperationStep(bound, run.WorkspaceID, run.BuildRunID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.Cancel(ctx, run.WorkspaceID, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishOperationStep(bound, second.WorkspaceID, second.BuildRunID, second.RevisionNo,
		second.OperationID, OperationStatusSucceeded, outputHash, "", "", json.RawMessage(`{"published":true}`), tasks.ValidateCurrentTaskTx); err == nil {
		t.Fatal("cancelled platform claim persisted stale compiler progress")
	}
	steps, err := store.ListOperationSteps(ctx, run.WorkspaceID, run.BuildRunID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Status != OperationStatusSucceeded || steps[1].Status != OperationStatusPending {
		t.Fatalf("compiler business projection = %#v", steps)
	}

	var retired *string
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('weave_team_build_operation_attempts')::text`).Scan(&retired); err != nil || retired != nil {
		t.Fatalf("private compiler attempt queue retained: %v %v", retired, err)
	}
}
