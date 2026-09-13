package teamrun

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

// A reclaimed member waits for an explicit user continuation. The checkpoint
// retains the logical invocation; parking fences the departed parent owner.
func (e *Executor) parkInterruptedMember(ctx context.Context, run TeamRun, task *taskqueue.Task, workerID, executorID string, checkpoint WorkflowCheckpointV1) error {
	detail, err := json.Marshal(RuntimeWaitDetailV1{SchemaVersion: 1, WaitType: "runtime", NodeID: checkpoint.NodeID})
	if err != nil {
		return err
	}
	parked, err := e.parkRunning(ctx, run, task, executorID, RuntimePark{
		NodeID: checkpoint.NodeID, WaitKind: WaitRuntime, WaitDetail: detail,
		ActiveMember: checkpoint.ActiveMember, MemberBreakdown: checkpoint.MemberBreakdown,
		CompletedOutputs: checkpoint.CompletedOutputs, ArtifactTaskIDs: checkpoint.ArtifactTaskIDs,
		DeliveryErrors: checkpoint.DeliveryErrors, Corrections: checkpoint.Corrections,
		UsageCheckpoint: checkpoint.Usage, UsageComplete: checkpoint.UsageComplete,
		UsageIncompleteReason: checkpoint.UsageIncompleteReason,
	})
	if err != nil {
		return err
	}
	return e.finishParkedTask(ctx, task, workerID, parked)
}

// MembersStoppedTx is the same durable stop test used by the recovery API
// and its command handler. An expired owner is fenced at the next admission.
func MembersStoppedTx(ctx context.Context, tx pgx.Tx, workspaceID, parentRunID string) (bool, error) {
	var stopped bool
	err := tx.QueryRow(ctx, `SELECT NOT EXISTS (
		SELECT 1 FROM weave_workflow_member_runs member JOIN weave_run_attempt_leases lease
		ON lease.workspace_id=member.workspace_id AND lease.run_id=member.member_run_id
		WHERE member.workspace_id=$1 AND member.parent_run_id=$2 AND member.result IS NULL
		AND lease.state='active' AND lease.lease_expires_at>statement_timestamp())`, workspaceID, parentRunID).Scan(&stopped)
	return stopped, err
}
