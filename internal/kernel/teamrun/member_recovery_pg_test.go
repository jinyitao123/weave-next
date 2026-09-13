package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"testing"
)

func withPendingMember(t *testing.T, cp WorkflowCheckpointV1) WorkflowCheckpointV1 {
	t.Helper()
	// Fixed wire fixture for a pending first call; integration tests exercise the allocator.
	h := sha256.New()
	h.Write([]byte("weave-usage-call-v1"))
	for _, part := range []string{cp.RunID, cp.NodeID} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write([]byte(part))
	}
	h.Write(make([]byte, 8))
	call := fmt.Sprintf("uc1_%x", h.Sum(nil))
	cp.Usage, _ = json.Marshal(map[string]any{"schema_version": 2, "next_call_ordinals": map[string]int{cp.NodeID: 1}, "calls": []any{map[string]any{"usage_call_id": call, "run_id": cp.RunID, "step": cp.NodeID, "call_ordinal": 0, "attempts": []any{}}}})
	cp.ActiveMember = &ActiveMemberInvocation{NodeID: cp.NodeID, CallID: call}
	return cp
}

func TestReclaimedMemberWaitsForExplicitContinuationRealPG(t *testing.T) {
	h := newProcessNextHarness(t)
	taskID, run := h.seedRunningWorkflowTask(t, "reclaimed-member")
	runtime := &WorkflowSerialRuntime{Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore()}
	cp := withPendingMember(t, checkpointFromPark(run, RuntimePark{NodeID: "lead", CompletedOutputs: map[string]json.RawMessage{}, UsageComplete: true}, h.now))
	writer := runtime.progressCheckpointWriter(run)
	if err := writer(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.executor.ProcessNext(t.Context(), "after-restart"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	parked := h.readRun(t, run.RunID)
	if parked.Status != StatusParked || parked.WaitKind == nil || *parked.WaitKind != WaitRuntime || h.runtime.resumeCalls != 0 || h.runtime.executeCalls != 0 {
		t.Fatalf("restart ran automatically: %+v", parked)
	}
	h.assertTask(t, taskID, taskqueue.StatusCompleted, run.RunID, "")
	if err := writer(t.Context(), cp); !errors.Is(err, errTaskLeaseLost) {
		t.Fatalf("old writer=%v", err)
	}
	retained := retryCheckpoint(t, h, parked)
	if retained.ActiveMember.CallID != cp.ActiveMember.CallID {
		t.Fatal("lost member identity")
	}
	s := &StageRetryService{Transactions: h.pool, Runs: NewPGStore(), Checkpoints: NewPGCheckpointStore(), Tasks: h.tasks, Now: h.executor.Now}
	request := StageRetryRequest{WorkspaceID: run.WorkspaceID, RunID: run.RunID, NodeID: cp.NodeID, IdempotencyKey: "continue-after-restart"}
	if _, err := s.Retry(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"done":true}`), UsageComplete: true}
	if ok, err := h.executor.ProcessNext(t.Context(), "user-continued"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if h.runtime.resumeCalls != 1 || h.readRun(t, run.RunID).Status != StatusSucceeded {
		t.Fatal("explicit continue did not execute")
	}
}

func TestMemberContinuationReclaimRequiresAnotherUserCommandRealPG(t *testing.T) {
	h, parked, s, request := seedRuntimeRetry(t)
	cp := withPendingMember(t, retryCheckpoint(t, h, parked))
	tx := h.mustBeginTx(t)
	if _, err := h.executor.Checkpoints.PutTx(t.Context(), tx, cp); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	request.IdempotencyKey = "first-continue"
	if _, err := s.Retry(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	h.runtime.resumeErr = context.Canceled
	if _, err := h.executor.ProcessNext(t.Context(), "lost-worker"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first=%v", err)
	}
	if _, err := h.pool.Exec(t.Context(), `UPDATE weave_task_queue SET status='queued',worker_id=NULL,lease_expires_at=NULL WHERE source='runtime_retry'`); err != nil {
		t.Fatal(err)
	}
	h.runtime.resumeErr = nil
	if ok, err := h.executor.ProcessNext(t.Context(), "replacement"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if h.runtime.resumeCalls != 1 || h.readRun(t, parked.RunID).Status != StatusParked {
		t.Fatal("reclaimed continuation executed without a second command")
	}
	request.IdempotencyKey = "second-continue"
	if _, err := s.Retry(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	h.runtime.resumeResult = RuntimeResult{Status: RuntimeCompleted, Output: json.RawMessage(`{"done":true}`), UsageComplete: true}
	if ok, err := h.executor.ProcessNext(t.Context(), "second-user-continue"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if h.runtime.resumeCalls != 2 {
		t.Fatal("new explicit continuation did not execute")
	}
}

func TestStageRetryRejectsUnknownToolOutcomeRealPG(t *testing.T) {
	h, parked, s, request := seedRuntimeRetry(t)
	if _, err := h.pool.Exec(t.Context(), `UPDATE weave_team_runs SET wait_detail=$1 WHERE workspace_id=$2 AND run_id=$3`, `{"schema_version":1,"wait_type":"runtime","node_id":"deliver","recovery_blocked":true}`, parked.WorkspaceID, parked.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Retry(t.Context(), request); !errors.Is(err, ErrTeamRunResumeInvalid) {
		t.Fatalf("unknown tool outcome accepted: %v", err)
	}
	if h.readRun(t, parked.RunID).Status != StatusParked {
		t.Fatal("blocked recovery mutated run")
	}
}
