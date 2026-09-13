package capabilities

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func capabilityTestContext(ctx context.Context, workspaceID string) context.Context {
	return execution.WithSubject(ctx, execution.Subject{WorkspaceID: workspaceID, UserID: "test-user"})
}

func executionFixture(t *testing.T) (*pgxpool.Pool, *PGStore, Invocation) {
	t.Helper()
	ctx := capabilityTestContext(t.Context(), "ws")
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := NewPGStore(pool)
	service := NewService(store, store)
	d := capability.Definition{SchemaVersion: 1, CapabilityID: "cap", Name: "cap", InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Roles: []capability.Role{{ID: "r", Name: "r"}}, Steps: []capability.Step{{ID: "s", Name: "s", RoleID: "r", Kind: capability.StepWorker, Instruction: "run"}}}
	if err := service.SaveDraft(ctx, DraftRequest{WorkspaceID: "ws", Definition: d}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(ctx, "ws", "cap", 1); err != nil {
		t.Fatal(err)
	}
	i, _, err := service.Invoke(ctx, InvokeRequest{WorkspaceID: "ws", ApplicationID: "app", CapabilityID: "cap", Revision: 1, RequestID: "req", Input: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	return pool, store, i
}

// These test helpers exercise legacy assertions through the platform queue;
// production has no capability-owned claim or completion entry point.
func (s *PGStore) ClaimTask(ctx context.Context) (InvocationTask, bool, error) {
	physical, err := s.queue.Claim(ctx, uuid.NewString(), taskqueue.ClaimFilter{Kind: "capability_invocation", IdentityKind: taskqueue.IdentityCapability})
	if err != nil || physical == nil {
		return InvocationTask{}, false, err
	}
	bound, err := taskqueue.BindTaskSubject(ctx, physical)
	if err != nil {
		return InvocationTask{}, false, err
	}
	task, err := s.LoadPlatformTask(bound, *physical)
	if err != nil {
		_ = s.queue.FailClaimed(bound, physical.ID, physical.WorkerID, err.Error())
		_ = s.queue.AcknowledgeExecutionStopped(bound, physical.ID, physical.WorkerID)
		return InvocationTask{}, false, nil
	}
	return task, true, nil
}

func (s *PGStore) CompleteTask(ctx context.Context, task InvocationTask, result json.RawMessage, executeErr error) (Invocation, error) {
	physical, err := s.queue.Get(ctx, task.WorkspaceID, task.TaskID)
	if err != nil {
		return Invocation{}, err
	}
	bound, err := taskqueue.BindTaskSubject(ctx, physical)
	if err != nil {
		return Invocation{}, err
	}
	if active, activeErr := s.TaskActive(bound, task); activeErr != nil || !active {
		_ = s.queue.FailClaimed(bound, task.TaskID, task.WorkerID, ErrAccessDenied.Error())
		_ = s.queue.AcknowledgeExecutionStopped(bound, task.TaskID, task.WorkerID)
		var applicationID string
		_ = s.pool.QueryRow(bound, `SELECT application_id FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID).Scan(&applicationID)
		return s.GetInvocation(bound, task.WorkspaceID, applicationID, task.InvocationID)
	}
	var pause *capability.PauseError
	if errors.As(executeErr, &pause) {
		if err := s.recordHumanPause(bound, task, pause); err != nil {
			return Invocation{}, err
		}
		err = s.queue.CompleteClaimed(bound, task.TaskID, task.WorkerID, nil, "")
	} else if executeErr != nil {
		err = s.queue.FailClaimed(bound, task.TaskID, task.WorkerID, executeErr.Error())
	} else {
		err = s.queue.CompleteClaimed(bound, task.TaskID, task.WorkerID, result, "")
	}
	if err != nil {
		_ = s.queue.AcknowledgeExecutionStopped(bound, task.TaskID, task.WorkerID)
		return Invocation{}, ErrClaimLost
	}
	_ = s.queue.AcknowledgeExecutionStopped(bound, task.TaskID, task.WorkerID)
	var applicationID string
	if err := s.pool.QueryRow(bound, `SELECT application_id FROM weave_capability_invocations WHERE workspace_id=$1 AND invocation_id=$2`, task.WorkspaceID, task.InvocationID).Scan(&applicationID); err != nil {
		return Invocation{}, err
	}
	return s.GetInvocation(bound, task.WorkspaceID, applicationID, task.InvocationID)
}
