package fanout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

var ErrYieldedCheckpointUnavailable = errors.New("yielded fanout checkpoint is unavailable")

type FanoutCoordinator interface {
	PreparePark(context.Context, pgx.Tx, PrepareParkRequest) (ParkIntent, error)
	PrepareFreeCollabSynthesis(context.Context, pgx.Tx, PrepareParkRequest) (ParkIntent, error)
	ActivatePark(context.Context, ActivateParkRequest) (ActivationResult, error)
	RecordLegCompletion(context.Context, LegCompletionRequest) (LegCompletionResult, error)
	ReconcileIntent(context.Context, string, string) error
	ReconcileGroup(context.Context, string, string) error
}

type CheckpointReader interface {
	GetYieldedCheckpoint(context.Context, string, string, int64) (YieldedCheckpoint, error)
}

type ParentRunResumer interface {
	ResumeParkedRun(context.Context, ResumeParkedRunRequest) (ResumeResult, error)
}

type resumeAttemptPlanner interface {
	PlanResumeAttempt(context.Context, string, string, int64) (string, error)
}

type CreatorLeaseReader interface {
	CreatorLeaseState(context.Context, CreatorLeaseIdentity) (CreatorLeaseState, error)
}

type LateSynthesisScheduler interface {
	ScheduleFromCompletion(context.Context, GroupCompletion) (LateSynthesisResult, error)
}

type coordinatorTransactions interface {
	Begin(context.Context) (pgx.Tx, error)
}

type workflowTaskStore interface {
	EnqueueTx(context.Context, pgx.Tx, *taskqueue.Task) error
	Cancel(context.Context, string, string) error
	RequestCancelTask(context.Context, string, string) (bool, error)
}

type WorkflowCoordinator struct {
	Transactions      coordinatorTransactions
	Store             *Store
	Checkpoints       CheckpointReader
	Resumer           ParentRunResumer
	CreatorLeases     CreatorLeaseReader
	Synthesis         LateSynthesisScheduler
	Tasks             workflowTaskStore
	Now               func() time.Time
	CancellationGrace time.Duration
}

func (c *WorkflowCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *WorkflowCoordinator) validateCore() error {
	if c == nil || c.Transactions == nil || c.Store == nil || c.Checkpoints == nil {
		return workflowError(ErrorStoreUnavailable, "workflow coordinator dependencies are unavailable")
	}
	return nil
}

func (c *WorkflowCoordinator) PreparePark(ctx context.Context, tx pgx.Tx, req PrepareParkRequest) (ParkIntent, error) {
	if err := c.validateCore(); err != nil {
		return ParkIntent{}, err
	}
	return c.Store.CreateParkIntentTx(ctx, tx, req, WorkflowResumeMode)
}

func (c *WorkflowCoordinator) PrepareFreeCollabSynthesis(
	ctx context.Context,
	tx pgx.Tx,
	req PrepareParkRequest,
) (ParkIntent, error) {
	if err := c.validateCore(); err != nil {
		return ParkIntent{}, err
	}
	return c.Store.CreateParkIntentTx(ctx, tx, req, FreeCollabSynthesisMode)
}

func (c *WorkflowCoordinator) ActivatePark(ctx context.Context, req ActivateParkRequest) (ActivationResult, error) {
	if err := c.validateCore(); err != nil {
		return ActivationResult{}, err
	}
	checkpoint, err := c.Checkpoints.GetYieldedCheckpoint(
		ctx, req.WorkspaceID, req.ParentRunID, req.CheckpointSequence,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := validateActivationCheckpoint(req, checkpoint); err != nil {
		return ActivationResult{}, err
	}
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return ActivationResult{}, workflowError(ErrorStoreUnavailable, "begin fanout activation: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	intent, err := c.Store.GetIntentTx(ctx, tx, req.WorkspaceID, req.IntentID)
	if err != nil {
		return ActivationResult{}, err
	}
	if intent.ParentRunID != req.ParentRunID || intent.Generation != req.Generation {
		return ActivationResult{}, workflowError(ErrorGenerationMismatch, "activation intent identity differs")
	}
	var result ActivationResult
	switch intent.Status {
	case IntentPending:
		result, err = c.Store.CASActivateIntentTx(ctx, tx, req.WorkspaceID, req.IntentID,
			req.Generation, req.CheckpointSequence, c.now())
	case IntentVoided:
		result, err = c.Store.CASReviveIntentTx(ctx, tx, req.WorkspaceID, req.IntentID,
			req.Generation, req.CheckpointSequence, c.now())
	case IntentActive:
		result, err = c.Store.CASActivateIntentTx(ctx, tx, req.WorkspaceID, req.IntentID,
			req.Generation, req.CheckpointSequence, c.now())
	default:
		err = workflowError(ErrorGenerationMismatch, "unknown intent status %q", intent.Status)
	}
	if err != nil {
		return ActivationResult{}, err
	}
	if result.Status != ActivationAlreadyActive {
		if c.Tasks == nil {
			return ActivationResult{}, workflowError(ErrorStoreUnavailable, "fanout task store is unavailable")
		}
		计划, err := c.Store.WorkflowLegPlansTx(ctx, tx, req.WorkspaceID, result.GroupID)
		if err != nil {
			return ActivationResult{}, err
		}
		for _, plan := range 计划 {
			task, err := workflowLegTask(intent, plan)
			if err != nil {
				return ActivationResult{}, err
			}
			if err := c.Tasks.EnqueueTx(ctx, tx, task); err != nil {
				return ActivationResult{}, workflowError(ErrorStoreUnavailable, "enqueue fanout leg: %v", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ActivationResult{}, workflowError(ErrorStoreUnavailable, "commit fanout activation: %v", err)
	}
	return result, nil
}

func validateActivationCheckpoint(req ActivateParkRequest, checkpoint YieldedCheckpoint) error {
	payload := checkpoint.Payload
	if checkpoint.WorkspaceID != req.WorkspaceID || checkpoint.ParentRunID != req.ParentRunID ||
		checkpoint.Sequence != req.CheckpointSequence || payload.WaitType != "fanout_group" ||
		!payload.Parked || payload.ParentRunID != req.ParentRunID || payload.IntentID != req.IntentID ||
		payload.Generation != req.Generation || payload.ResumeToken != req.ResumeToken || payload.GroupID == "" {
		return workflowError(ErrorGenerationMismatch, "yielded checkpoint does not match activation request")
	}
	return nil
}

type workflowLegTaskPayloadV1 struct {
	SchemaVersion   int             `json:"schema_version"`
	Kind            string          `json:"kind"`
	WorkspaceID     string          `json:"workspace_id"`
	ParentRunID     string          `json:"parent_run_id"`
	IntentID        string          `json:"intent_id"`
	GroupID         string          `json:"group_id"`
	LegID           string          `json:"leg_id"`
	BranchID        string          `json:"branch_id"`
	BranchOrdinal   int             `json:"branch_ordinal"`
	Generation      string          `json:"generation"`
	FrozenBundleRef json.RawMessage `json:"frozen_bundle_ref"`
	InputRef        json.RawMessage `json:"input_ref"`
	MayYieldProof   json.RawMessage `json:"may_yield_proof"`
}

func workflowLegTask(intent ParkIntent, leg WorkflowLegPlan) (*taskqueue.Task, error) {
	taskID, err := DeriveLegTaskID(leg.GroupID, leg.BranchID, leg.Generation)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(workflowLegTaskPayloadV1{
		SchemaVersion: 1, Kind: "fanout_leg", WorkspaceID: intent.WorkspaceID,
		ParentRunID: intent.ParentRunID, IntentID: intent.IntentID, GroupID: leg.GroupID,
		LegID: leg.LegID, BranchID: leg.BranchID, BranchOrdinal: leg.BranchOrdinal,
		Generation: leg.Generation, FrozenBundleRef: leg.FrozenBundleRef,
		InputRef: leg.InputRef, MayYieldProof: leg.MayYieldProof,
	})
	if err != nil {
		return nil, workflowError(ErrorInvalidRequest, "encode fanout leg task: %v", err)
	}
	return &taskqueue.Task{
		ID: taskID, WorkspaceID: intent.WorkspaceID, IdentityKind: taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2, WorkflowID: intent.WorkflowID,
		WorkflowVersion: int(intent.WorkflowVersion), RunSnapshotID: intent.RunSnapshotID,
		Source: "fanout", Kind: "team_workflow", ContextKey: leg.GroupID, Payload: payload,
	}, nil
}

func (c *WorkflowCoordinator) RecordLegCompletion(ctx context.Context, req LegCompletionRequest) (LegCompletionResult, error) {
	if err := c.validateCore(); err != nil {
		return LegCompletionResult{}, err
	}
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return LegCompletionResult{}, workflowError(ErrorStoreUnavailable, "begin leg completion: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := c.Store.RecordLegTerminalTx(ctx, tx, req)
	if err != nil {
		return LegCompletionResult{}, err
	}
	var evaluation JoinEvaluation
	var decided bool
	if result.Disposition == LegCompletionApplied {
		evaluation, decided, err = c.Store.TryDecideGroupTx(
			ctx, tx, req.WorkspaceID, req.GroupID, c.now(), c.CancellationGrace,
		)
		if ErrorCodeOf(err) == ErrorGroupAlreadyDecided {
			err = nil
		}
		if err != nil {
			return LegCompletionResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return LegCompletionResult{}, workflowError(ErrorStoreUnavailable, "commit leg completion: %v", err)
	}
	if decided {
		if err := c.applyTaskCancellation(ctx, req.WorkspaceID, req.GroupID, req.Generation, evaluation.Actions); err != nil {
			return LegCompletionResult{}, err
		}
	}
	return result, nil
}

func (c *WorkflowCoordinator) ReconcileIntent(ctx context.Context, workspaceID, intentID string) error {
	if err := c.validateCore(); err != nil {
		return err
	}
	intent, err := c.Store.GetIntent(ctx, workspaceID, intentID)
	if err != nil {
		return err
	}
	sequence := intent.PreviousCheckpointSequence + 1
	checkpoint, checkpointErr := c.Checkpoints.GetYieldedCheckpoint(ctx, workspaceID, intent.ParentRunID, sequence)
	if checkpointErr == nil {
		return c.activateFromCheckpoint(ctx, intent, checkpoint)
	}
	if !errors.Is(checkpointErr, ErrYieldedCheckpointUnavailable) {
		return checkpointErr
	}
	if intent.Status != IntentPending {
		return nil
	}
	if c.CreatorLeases == nil {
		return workflowError(ErrorStoreUnavailable, "creator lease reader is unavailable")
	}
	lease, err := c.CreatorLeases.CreatorLeaseState(ctx, CreatorLeaseIdentity{
		WorkspaceID: workspaceID, RunID: intent.ParentRunID, CreatorEpoch: intent.CreatorEpoch,
		AttemptGeneration: intent.CreatorAttemptGeneration, AttemptID: intent.CreatorAttemptID,
	})
	if err != nil {
		return err
	}
	terminal := lease.MarkerPhase == "final"
	expired := !lease.LeaseExpiresAt.IsZero() && !lease.LeaseExpiresAt.After(c.now())
	if !terminal && !expired {
		return nil
	}
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return workflowError(ErrorStoreUnavailable, "begin void intent: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := c.Store.GetIntentTx(ctx, tx, workspaceID, intentID)
	if err != nil {
		return err
	}
	if locked.Status == IntentPending {
		if _, err := c.Store.CASVoidIntentTx(ctx, tx, workspaceID, intentID, locked.Generation, c.now()); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowError(ErrorStoreUnavailable, "commit void intent: %v", err)
	}
	return nil
}

func (c *WorkflowCoordinator) activateFromCheckpoint(ctx context.Context, intent ParkIntent, checkpoint YieldedCheckpoint) error {
	payload := checkpoint.Payload
	if payload.IntentID != intent.IntentID || payload.Generation != intent.Generation ||
		payload.ParentRunID != intent.ParentRunID {
		return workflowError(ErrorGenerationMismatch, "reconciled checkpoint identity differs")
	}
	_, err := c.ActivatePark(ctx, ActivateParkRequest{
		WorkspaceID: intent.WorkspaceID, IntentID: intent.IntentID, ParentRunID: intent.ParentRunID,
		CheckpointSequence: checkpoint.Sequence, Generation: intent.Generation,
		ResumeToken: payload.ResumeToken,
	})
	return err
}

func (c *WorkflowCoordinator) ReconcileGroup(ctx context.Context, workspaceID, groupID string) error {
	if err := c.validateCore(); err != nil {
		return err
	}
	if err := c.tryDecideGroup(ctx, workspaceID, groupID); err != nil {
		return err
	}
	plan, err := c.readResumePlan(ctx, workspaceID, groupID)
	if err != nil {
		return err
	}
	if plan.Status == WorkflowGroupResumed || plan.Status == WorkflowGroupClosed {
		return nil
	}
	if plan.Status != WorkflowGroupDecided {
		return nil
	}
	if plan.Mode == FreeCollabSynthesisMode {
		if c.Synthesis == nil {
			return nil
		}
		_, err := c.Synthesis.ScheduleFromCompletion(ctx, GroupCompletion{
			WorkspaceID: plan.WorkspaceID, GroupID: plan.GroupID,
			GroupCompletionID: plan.GroupCompletionID, Mode: string(plan.Mode),
			Generation: plan.Generation, JoinResult: plan.JoinResult,
		})
		return err
	}
	if c.Resumer == nil || c.CreatorLeases == nil {
		return workflowError(ErrorStoreUnavailable, "workflow resume dependencies are unavailable")
	}
	if plan.ResumeClaimState == nil {
		lease, err := c.CreatorLeases.CreatorLeaseState(ctx, CreatorLeaseIdentity{
			WorkspaceID: workspaceID, RunID: plan.ParentRunID, CreatorEpoch: plan.CreatorEpoch,
			AttemptGeneration: plan.CreatorAttemptGeneration, AttemptID: plan.CreatorAttemptID,
		})
		if err != nil {
			return err
		}
		if creatorAttemptFinalOrMoved(plan, lease) {
			return c.closeWorkflowResumeGroup(ctx, plan, "creator_attempt_unavailable_before_resume_claim")
		}
		claimID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("weave/fanout-resume-claim/v1\x00"+workspaceID+"\x00"+groupID+"\x00"+plan.GroupCompletionID)).String()
		if err := c.claimResume(ctx, plan, claimID, lease); err != nil {
			return err
		}
		plan, err = c.readResumePlan(ctx, workspaceID, groupID)
		if err != nil {
			return err
		}
	}
	if plan.CheckpointSequence == nil {
		return workflowError(ErrorResumeConflict, "decided group has no activation checkpoint")
	}
	checkpoint, err := c.Checkpoints.GetYieldedCheckpoint(
		ctx, workspaceID, plan.ParentRunID, *plan.CheckpointSequence,
	)
	if err != nil {
		if errors.Is(err, ErrYieldedCheckpointUnavailable) {
			closed, closeErr := c.closeWorkflowResumeGroupIfCreatorUnavailable(
				ctx, plan, "yielded_checkpoint_unavailable_after_resume_decision",
			)
			if closeErr != nil {
				return closeErr
			}
			if closed {
				return nil
			}
		}
		return err
	}
	newGeneration := plan.PreviousAttemptGeneration + 1
	newAttemptID := plan.NewAttemptID
	if plan.NewAttemptGeneration != nil {
		newGeneration = *plan.NewAttemptGeneration
	}
	if newAttemptID == "" {
		planner, ok := c.Resumer.(resumeAttemptPlanner)
		if !ok {
			return workflowError(ErrorStoreUnavailable, "resume attempt planner is unavailable")
		}
		newAttemptID, err = planner.PlanResumeAttempt(ctx, workspaceID, plan.ParentRunID, newGeneration)
		if err != nil {
			return err
		}
	}
	result, err := c.Resumer.ResumeParkedRun(ctx, ResumeParkedRunRequest{
		WorkspaceID: workspaceID, ParentRunID: plan.ParentRunID, RunSnapshotID: plan.RunSnapshotID,
		IntentID: plan.IntentID, GroupID: groupID, Generation: plan.Generation,
		ResumeToken: checkpoint.Payload.ResumeToken, GroupCompletionID: plan.GroupCompletionID,
		ClaimID: plan.ResumeClaimID, ExpectedAttemptGeneration: plan.PreviousAttemptGeneration,
		ExpectedAttemptID: plan.PreviousAttemptID, NewAttemptGeneration: newGeneration,
		NewAttemptID: newAttemptID, JoinResult: plan.JoinResult,
	})
	if err != nil {
		return err
	}
	if result.Status == ResumeClaimConflict {
		return workflowError(ErrorResumeConflict, "parent resume claim conflicted")
	}
	return c.advanceResume(ctx, plan, result)
}

func (c *WorkflowCoordinator) closeWorkflowResumeGroupIfCreatorUnavailable(
	ctx context.Context,
	plan WorkflowResumePlan,
	reason string,
) (bool, error) {
	lease, err := c.CreatorLeases.CreatorLeaseState(ctx, CreatorLeaseIdentity{
		WorkspaceID: plan.WorkspaceID, RunID: plan.ParentRunID, CreatorEpoch: plan.CreatorEpoch,
		AttemptGeneration: plan.CreatorAttemptGeneration, AttemptID: plan.CreatorAttemptID,
	})
	if err != nil {
		if ErrorCodeOf(err) == ErrorResumeConflict {
			return true, c.closeWorkflowResumeGroup(ctx, plan, reason+"_creator_lease_conflict")
		}
		return false, err
	}
	if !creatorAttemptUnavailableAfterMissingCheckpoint(plan, lease, c.now()) {
		return false, nil
	}
	return true, c.closeWorkflowResumeGroup(ctx, plan, reason)
}

func creatorAttemptFinalOrMoved(plan WorkflowResumePlan, lease CreatorLeaseState) bool {
	return lease.MarkerPhase == "final" ||
		lease.AttemptGeneration != plan.CreatorAttemptGeneration ||
		lease.AttemptID != plan.CreatorAttemptID
}

func creatorAttemptUnavailableAfterMissingCheckpoint(plan WorkflowResumePlan, lease CreatorLeaseState, now time.Time) bool {
	expired := !lease.LeaseExpiresAt.IsZero() && !lease.LeaseExpiresAt.After(now)
	return creatorAttemptFinalOrMoved(plan, lease) || expired
}

func (c *WorkflowCoordinator) closeWorkflowResumeGroup(
	ctx context.Context,
	plan WorkflowResumePlan,
	reason string,
) error {
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return workflowError(ErrorStoreUnavailable, "begin workflow resume close: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := c.Store.GetWorkflowResumePlanTx(ctx, tx, plan.WorkspaceID, plan.GroupID)
	if err != nil {
		return err
	}
	if locked.Status == WorkflowGroupClosed || locked.Status == WorkflowGroupResumed {
		if err := tx.Commit(ctx); err != nil {
			return workflowError(ErrorStoreUnavailable, "commit workflow resume close replay: %v", err)
		}
		return nil
	}
	if locked.Mode != WorkflowResumeMode ||
		locked.Status != WorkflowGroupDecided ||
		locked.Generation != plan.Generation ||
		locked.GroupCompletionID != plan.GroupCompletionID {
		return workflowError(ErrorResumeConflict, "workflow resume close identity differs")
	}
	if _, err := c.Store.CASCloseWorkflowResumeGroupTx(ctx, tx, CloseWorkflowResumeGroupRequest{
		WorkspaceID: locked.WorkspaceID, GroupID: locked.GroupID, Generation: locked.Generation,
		GroupCompletionID: locked.GroupCompletionID, Reason: reason, ClosedAt: c.now(),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowError(ErrorStoreUnavailable, "commit workflow resume close: %v", err)
	}
	return nil
}

func (c *WorkflowCoordinator) tryDecideGroup(ctx context.Context, workspaceID, groupID string) error {
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return workflowError(ErrorStoreUnavailable, "begin group decision: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	evaluation, decided, err := c.Store.TryDecideGroupTx(
		ctx, tx, workspaceID, groupID, c.now(), c.CancellationGrace,
	)
	if ErrorCodeOf(err) == ErrorGroupAlreadyDecided {
		return nil
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowError(ErrorStoreUnavailable, "commit group decision: %v", err)
	}
	if decided {
		return c.applyTaskCancellation(ctx, workspaceID, groupID, evaluation.Frozen.Generation, evaluation.Actions)
	}
	return nil
}

func (c *WorkflowCoordinator) readResumePlan(ctx context.Context, workspaceID, groupID string) (WorkflowResumePlan, error) {
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return WorkflowResumePlan{}, workflowError(ErrorStoreUnavailable, "begin resume plan read: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plan, err := c.Store.GetWorkflowResumePlanTx(ctx, tx, workspaceID, groupID)
	if err != nil {
		return WorkflowResumePlan{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkflowResumePlan{}, workflowError(ErrorStoreUnavailable, "commit resume plan read: %v", err)
	}
	return plan, nil
}

func (c *WorkflowCoordinator) claimResume(ctx context.Context, plan WorkflowResumePlan, claimID string, lease CreatorLeaseState) error {
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return workflowError(ErrorStoreUnavailable, "begin resume claim: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, _, err := c.Store.CASClaimResumeTx(ctx, tx, ClaimResumeRequest{
		WorkspaceID: plan.WorkspaceID, GroupID: plan.GroupID,
		GroupCompletionID: plan.GroupCompletionID, ClaimID: claimID,
		PreviousAttemptGeneration: lease.AttemptGeneration, PreviousAttemptID: lease.AttemptID,
		ClaimedAt: c.now(),
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowError(ErrorStoreUnavailable, "commit resume claim: %v", err)
	}
	return nil
}

func (c *WorkflowCoordinator) advanceResume(ctx context.Context, plan WorkflowResumePlan, result ResumeResult) error {
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return workflowError(ErrorStoreUnavailable, "begin resume advance: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	advancedAt := c.now()
	var receipt resumeReceiptV1
	if err := json.Unmarshal(result.ResumeReceipt, &receipt); err == nil && !receipt.AdvancedAt.IsZero() {
		advancedAt = receipt.AdvancedAt
	}
	_, _, err = c.Store.CASAdvanceResumeTx(ctx, tx, AdvanceResumeRequest{
		WorkspaceID: plan.WorkspaceID, GroupID: plan.GroupID, ClaimID: result.ClaimID,
		NewAttemptGeneration: result.AttemptGeneration, NewAttemptID: result.AttemptID,
		ResumeReceiptID: result.ResumeReceiptID, ResumeReceipt: result.ResumeReceipt,
		AdvancedAt: advancedAt,
	})
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowError(ErrorStoreUnavailable, "commit resume advance: %v", err)
	}
	return nil
}

func (c *WorkflowCoordinator) applyTaskCancellation(ctx context.Context, workspaceID, groupID, generation string, actions JoinActions) error {
	if c.Tasks == nil || (len(actions.CutLegIDs) == 0 && len(actions.RequestCancelLegIDs) == 0) {
		return nil
	}
	tx, err := c.Transactions.Begin(ctx)
	if err != nil {
		return workflowError(ErrorStoreUnavailable, "begin cancellation plan read: %v", err)
	}
	计划, err := c.Store.WorkflowLegPlansTx(ctx, tx, workspaceID, groupID)
	_ = tx.Rollback(ctx)
	if err != nil {
		return err
	}
	byLeg := make(map[string]WorkflowLegPlan, len(计划))
	for _, plan := range 计划 {
		byLeg[plan.LegID] = plan
	}
	for _, legID := range actions.CutLegIDs {
		plan, ok := byLeg[legID]
		if !ok {
			return workflowError(ErrorInvalidRequest, "cut leg %q has no durable plan", legID)
		}
		taskID, _ := DeriveLegTaskID(groupID, plan.BranchID, generation)
		if err := c.Tasks.Cancel(ctx, workspaceID, taskID); err != nil {
			return workflowError(ErrorStoreUnavailable, "cut fanout task: %v", err)
		}
	}
	for _, legID := range actions.RequestCancelLegIDs {
		plan, ok := byLeg[legID]
		if !ok {
			return workflowError(ErrorInvalidRequest, "cancel leg %q has no durable plan", legID)
		}
		taskID, _ := DeriveLegTaskID(groupID, plan.BranchID, generation)
		if _, err := c.Tasks.RequestCancelTask(ctx, workspaceID, taskID); err != nil {
			return workflowError(ErrorStoreUnavailable, "request fanout task cancellation: %v", err)
		}
	}
	return nil
}

func NewWorkflowCoordinator(transactions coordinatorTransactions, store *Store, checkpoints CheckpointReader) *WorkflowCoordinator {
	return &WorkflowCoordinator{
		Transactions: transactions, Store: store, Checkpoints: checkpoints,
		CancellationGrace: 30 * time.Second,
	}
}

func resumeConflict(format string, args ...any) (ResumeResult, error) {
	return ResumeResult{Status: ResumeClaimConflict}, workflowError(ErrorResumeConflict, format, args...)
}

func wrapStoreUnavailable(operation string, err error) error {
	return workflowError(ErrorStoreUnavailable, "%s: %v", operation, err)
}

func requireResumeIdentity(req ResumeParkedRunRequest) error {
	if req.WorkspaceID == "" || req.ParentRunID == "" || req.RunSnapshotID == "" ||
		req.IntentID == "" || req.GroupID == "" || req.Generation == "" || req.ResumeToken == "" ||
		req.GroupCompletionID == "" || req.ClaimID == "" || req.ExpectedAttemptGeneration < 1 ||
		req.ExpectedAttemptID == "" || req.NewAttemptGeneration != req.ExpectedAttemptGeneration+1 ||
		req.NewAttemptID == "" || !json.Valid(req.JoinResult) {
		return fmt.Errorf("resume request identity is invalid")
	}
	return nil
}
