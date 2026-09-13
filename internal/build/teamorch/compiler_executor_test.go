package teamorch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/build/teamforge"
)

type oneStepCompilerStore struct {
	run      teambuild.TeamBuildRun
	revision teambuild.BlueprintRevision
	steps    []teambuild.OperationStep
	finishes int
}

func (s *oneStepCompilerStore) GetBuildRun(context.Context, string, string) (teambuild.TeamBuildRun, error) {
	return s.run, nil
}
func (s *oneStepCompilerStore) GetLatestBlueprintRevision(context.Context, string, string) (teambuild.BlueprintRevision, error) {
	return s.revision, nil
}
func (s *oneStepCompilerStore) ListOperationSteps(context.Context, string, string, int) ([]teambuild.OperationStep, error) {
	return append([]teambuild.OperationStep(nil), s.steps...), nil
}
func (s *oneStepCompilerStore) NextReadyOperationStep(context.Context, string, string, int) (teambuild.OperationStep, error) {
	for _, step := range s.steps {
		if step.Status == teambuild.OperationStatusPending {
			return step, nil
		}
	}
	return teambuild.OperationStep{}, teambuild.ErrNoReadyOperationStep
}
func (s *oneStepCompilerStore) FinishOperationStep(ctx context.Context, _, _ string, _ int, operationID, status, outputHash, errorClass, errorCode string, evidence json.RawMessage, fence teambuild.ExecutionFence) (teambuild.OperationStep, error) {
	if err := fence(ctx, nil); err != nil {
		return teambuild.OperationStep{}, err
	}
	for index := range s.steps {
		if s.steps[index].OperationID == operationID && s.steps[index].Status == teambuild.OperationStatusPending {
			s.finishes++
			s.steps[index].Status = status
			s.steps[index].OutputHash = outputHash
			s.steps[index].ErrorClass = errorClass
			s.steps[index].ErrorCode = errorCode
			s.steps[index].EvidenceJSON = evidence
			return s.steps[index], nil
		}
	}
	return teambuild.OperationStep{}, teambuild.ErrOperationStepConflict
}

type countingOperationHandler struct {
	calls    int
	attempts []int
}

func (h *countingOperationHandler) HandleOperation(_ context.Context, operation OperationContext) (OperationResult, error) {
	h.calls++
	h.attempts = append(h.attempts, operation.PhysicalAttempt)
	return OperationResult{Evidence: json.RawMessage(`{"done":true}`)}, nil
}

func TestCompilerExecutorRunsOneBusinessStepPerPlatformAttempt(t *testing.T) {
	blueprint, _ := json.Marshal(teambuild.TeamBlueprintV1{SchemaVersion: 1, Mode: teambuild.ModeCreate})
	changeSet, _ := json.Marshal(teamforge.ChangeSetV1{SchemaVersion: 1, Operations: []teamforge.ChangeOperationV1{
		{OperationID: "operation-1", Type: teamforge.OperationAgentCreate},
		{OperationID: "operation-2", Type: teamforge.OperationAgentCreate},
	}})
	store := &oneStepCompilerStore{
		run: teambuild.TeamBuildRun{WorkspaceID: "workspace-1", BuildRunID: "build-1", Status: teambuild.StatusRoundRunning},
		revision: teambuild.BlueprintRevision{WorkspaceID: "workspace-1", BuildRunID: "build-1", RevisionNo: 1,
			BlueprintJSON: blueprint, ChangeSetJSON: changeSet},
		steps: []teambuild.OperationStep{
			{WorkspaceID: "workspace-1", BuildRunID: "build-1", RevisionNo: 1, OperationID: "operation-1", OperationType: string(teamforge.OperationAgentCreate), Status: teambuild.OperationStatusPending},
			{WorkspaceID: "workspace-1", BuildRunID: "build-1", RevisionNo: 1, OperationID: "operation-2", OperationType: string(teamforge.OperationAgentCreate), Status: teambuild.OperationStatusPending},
		},
	}
	handler := &countingOperationHandler{}
	subject := execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}
	ctx, err := execution.WithCurrentTask(execution.WithSubject(context.Background(), subject), execution.CurrentTask{ID: "task-1", WorkspaceID: "workspace-1", Subject: subject, WorkerID: "worker-1", ClaimEpoch: 7})
	if err != nil {
		t.Fatal(err)
	}
	ctx = WithCompilerExecutionFence(ctx, func(context.Context, pgx.Tx) error { return nil })
	result, err := NewCompilerExecutor(store, handler).Execute(ctx, "workspace-1", "build-1")
	if err != nil {
		t.Fatal(err)
	}
	if handler.calls != 1 || store.finishes != 1 || len(handler.attempts) != 1 || handler.attempts[0] != 7 {
		t.Fatalf("one platform attempt executed calls=%d finishes=%d attempts=%v", handler.calls, store.finishes, handler.attempts)
	}
	if !result.Pending || result.Complete || store.steps[0].Status != teambuild.OperationStatusSucceeded || store.steps[1].Status != teambuild.OperationStatusPending {
		t.Fatalf("compiler result=%#v steps=%#v", result, store.steps)
	}
}
