package teamrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/fanout"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type FanoutCheckpointReader struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	Checkpoints  *PGCheckpointStore
}

func (r *FanoutCheckpointReader) GetYieldedCheckpoint(ctx context.Context, workspaceID, parentRunID string, sequence int64) (fanout.YieldedCheckpoint, error) {
	if r == nil || r.Transactions == nil || r.Runs == nil || r.Checkpoints == nil ||
		workspaceID == "" || parentRunID == "" || sequence < 0 {
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorInvalidRequest, "checkpoint read request is invalid")
	}
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "begin checkpoint read: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := r.Runs.GetForUpdateTx(ctx, tx, workspaceID, parentRunID)
	if errors.Is(err, ErrTeamRunIdentityMismatch) || (err == nil && (run.Status != StatusParked ||
		run.WaitKind == nil || *run.WaitKind != WaitFanout || int64(run.ResumeGeneration) != sequence)) {
		return fanout.YieldedCheckpoint{}, fanout.ErrYieldedCheckpointUnavailable
	}
	if err != nil {
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "read parked parent run: %v", err)
	}
	var payload fanout.FanoutWaitPayload
	if err := decodeFanoutExact(run.WaitDetail, &payload); err != nil || payload.WaitType != "fanout_group" ||
		!payload.Parked || payload.ParentRunID != parentRunID || payload.JoinNodeID == "" {
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorGenerationMismatch, "parked fanout wait detail is invalid")
	}
	tokenHash := sha256.Sum256([]byte(payload.ResumeToken))
	if !bytes.Equal(tokenHash[:], run.ResumeTokenHash) {
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorGenerationMismatch, "parked fanout resume token hash differs")
	}
	checkpoint, err := r.Checkpoints.GetTx(ctx, tx, workspaceID, parentRunID)
	if err != nil {
		if errors.Is(err, ErrWorkflowCheckpointMissing) {
			return fanout.YieldedCheckpoint{}, fanout.ErrYieldedCheckpointUnavailable
		}
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "read yielded workflow checkpoint: %v", err)
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil || checkpoint.NodeID != payload.JoinNodeID {
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorGenerationMismatch, "yielded workflow checkpoint identity differs")
	}
	if err := tx.Commit(ctx); err != nil {
		return fanout.YieldedCheckpoint{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "commit checkpoint read: %v", err)
	}
	return fanout.YieldedCheckpoint{
		WorkspaceID: workspaceID, ParentRunID: parentRunID, Sequence: sequence, Payload: payload,
	}, nil
}

type FanoutCreatorLeaseReader struct {
	Transactions TransactionBeginner
	Records      loomruntime.TerminalRecordStore
}

func (r *FanoutCreatorLeaseReader) CreatorLeaseState(ctx context.Context, identity fanout.CreatorLeaseIdentity) (fanout.CreatorLeaseState, error) {
	if r == nil || r.Transactions == nil || r.Records == nil || identity.WorkspaceID == "" ||
		identity.RunID == "" || identity.AttemptGeneration < 1 || identity.AttemptID == "" {
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorInvalidRequest, "creator lease identity is invalid")
	}
	attemptID, err := uuid.Parse(identity.AttemptID)
	if err != nil {
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorInvalidRequest, "creator attempt ID is invalid")
	}
	registry, err := loomruntime.NewExpectedRunRegistry(r.Records)
	if err != nil {
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "create expected run registry: %v", err)
	}
	expected, present, err := registry.Get(ctx, identity.WorkspaceID, identity.RunID)
	if err != nil || !present || expected.WorkspaceID != identity.WorkspaceID || expected.RunID != identity.RunID {
		if err == nil {
			err = errors.New("expected run is missing")
		}
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorResumeConflict, "creator expected run is unavailable: %v", err)
	}
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "begin creator lease read: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stateStore := loomruntime.NewPGTerminalStateStore()
	lease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(ctx, tx, identity.WorkspaceID, identity.RunID)
	if err != nil || !leasePresent || lease.AttemptGeneration != identity.AttemptGeneration || lease.AttemptID != attemptID {
		if err == nil {
			err = errors.New("attempt lease identity differs")
		}
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorResumeConflict, "creator attempt lease unavailable: %v", err)
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(ctx, tx, identity.WorkspaceID, identity.RunID)
	if err != nil {
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "read creator terminal marker: %v", err)
	}
	result := fanout.CreatorLeaseState{
		WorkspaceID: lease.WorkspaceID, RunID: lease.RunID, AttemptGeneration: lease.AttemptGeneration,
		AttemptID: lease.AttemptID.String(), State: string(lease.State), LeaseExpiresAt: lease.LeaseExpiresAt,
		MarkerPhase: "absent",
	}
	if markerPresent {
		if marker.AttemptGeneration != lease.AttemptGeneration || marker.AttemptID != lease.AttemptID {
			return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorResumeConflict, "creator terminal marker attempt differs")
		}
		result.MarkerPhase = string(marker.Phase)
		result.MarkerGeneration = marker.AttemptGeneration
		result.MarkerAttemptID = marker.AttemptID.String()
	}
	if err := tx.Commit(ctx); err != nil {
		return fanout.CreatorLeaseState{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "commit creator lease read: %v", err)
	}
	return result, nil
}

type FanoutParentRunResumer struct {
	Transactions TransactionBeginner
	Fanout       *fanout.Store
	Runs         *PGStore
	Checkpoints  *PGCheckpointStore
	Tasks        *taskqueue.Store
	Records      loomruntime.TerminalRecordStore
	Now          func() time.Time
	LeaseTTL     time.Duration
}

func (r *FanoutParentRunResumer) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *FanoutParentRunResumer) validate() error {
	if r == nil || r.Transactions == nil || r.Fanout == nil || r.Runs == nil ||
		r.Checkpoints == nil || r.Tasks == nil || r.Records == nil {
		return fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "parent run resumer dependencies are unavailable")
	}
	return nil
}

func (r *FanoutParentRunResumer) PlanResumeAttempt(ctx context.Context, workspaceID, parentRunID string, generation int64) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := r.Runs.GetForUpdateTx(ctx, tx, workspaceID, parentRunID)
	if err != nil {
		return "", err
	}
	if generation != int64(run.ExecutionLeaseEpoch)+1 && generation != int64(run.ExecutionLeaseEpoch) {
		return "", fanout.NewWorkflowError(fanout.ErrorResumeConflict, "resume attempt generation differs from parent epoch")
	}
	return frozenAttemptIDForEpoch(run, ExecutionLeaseEpoch(generation)).String(), nil
}

func (r *FanoutParentRunResumer) ResumeParkedRun(ctx context.Context, req fanout.ResumeParkedRunRequest) (fanout.ResumeResult, error) {
	if err := r.validate(); err != nil {
		return fanout.ResumeResult{}, err
	}
	if err := validateFanoutResumeRequest(req); err != nil {
		return fanout.ResumeResult{Status: fanout.ResumeClaimConflict}, err
	}
	if err := r.admitResume(ctx, req); err != nil {
		return fanout.ResumeResult{}, err
	}
	return r.advanceGraph(ctx, req)
}

func (r *FanoutParentRunResumer) admitResume(ctx context.Context, req fanout.ResumeParkedRunRequest) error {
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plan, err := r.Fanout.GetWorkflowResumePlanTx(ctx, tx, req.WorkspaceID, req.GroupID)
	if err != nil {
		return err
	}
	if err := validateFanoutResumePlan(plan, req); err != nil {
		return err
	}
	if plan.ResumeClaimState != nil && *plan.ResumeClaimState == fanout.ResumeClaimAdmitted {
		if plan.NewAttemptGeneration == nil || *plan.NewAttemptGeneration != req.NewAttemptGeneration || plan.NewAttemptID != req.NewAttemptID {
			return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "durable resume admission identity differs")
		}
		return tx.Commit(ctx)
	}
	if plan.ResumeClaimState == nil || *plan.ResumeClaimState != fanout.ResumeClaimClaimed {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "resume claim is not ready for admission")
	}
	run, err := r.Runs.GetForUpdateTx(ctx, tx, req.WorkspaceID, req.ParentRunID)
	if err != nil {
		return err
	}
	if err := validateFanoutParkedRun(run, req); err != nil {
		return err
	}
	checkpoint, err := r.Checkpoints.GetTx(ctx, tx, req.WorkspaceID, req.ParentRunID)
	if err != nil {
		return err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "resume checkpoint differs: %v", err)
	}
	stateStore := loomruntime.NewPGTerminalStateStore()
	lease, present, err := stateStore.ReadAttemptLeaseForUpdate(ctx, tx, req.WorkspaceID, req.ParentRunID)
	if err != nil || !present {
		if err == nil {
			err = errors.New("attempt lease is missing")
		}
		return err
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(ctx, tx, req.WorkspaceID, req.ParentRunID)
	if err != nil {
		return err
	}
	expectedAttemptID, _ := uuid.Parse(req.ExpectedAttemptID)
	if lease.AttemptGeneration != req.ExpectedAttemptGeneration || lease.AttemptID != expectedAttemptID ||
		lease.State != loomruntime.AttemptLeaseActive || !markerPresent ||
		marker.Phase != loomruntime.TerminalMarkerPhaseYielded || marker.AttemptGeneration != lease.AttemptGeneration ||
		marker.AttemptID != lease.AttemptID {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "parent attempt is not the exact yielded owner")
	}
	executorID := "teamrun-fanout-resume:" + req.ClaimID
	tokenHash := sha256.Sum256([]byte(req.ResumeToken))
	resumed, err := r.Runs.ResumeRunningTx(ctx, tx, ResumeRequest{
		WorkspaceID: req.WorkspaceID, RunID: req.ParentRunID, ExpectedStatus: StatusParked,
		ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration: run.ResumeGeneration, ExpectedWaitKind: WaitFanout,
		ExpectedResumeTokenHash: tokenHash[:], ExecutorID: executorID,
		IdempotencyKey: "teamrun-fanout-resume:" + req.ClaimID, Actor: executorID,
		Source: "teamrun_worker", OccurredAt: r.now(),
	})
	if err != nil {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "resume parent TeamRun: %v", err)
	}
	newAttemptID, err := uuid.Parse(req.NewAttemptID)
	if err != nil || int64(resumed.ExecutionLeaseEpoch) != req.NewAttemptGeneration ||
		frozenAttemptIDForEpoch(resumed, resumed.ExecutionLeaseEpoch) != newAttemptID {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "new attempt identity differs from resumed TeamRun")
	}
	ttl := r.LeaseTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	advanced, updated, err := loomruntime.AdvanceFrozenAttemptTx(ctx, tx, loomruntime.FrozenAttemptAdvance{
		Current: loomruntime.AttemptLeaseOwner{
			WorkspaceID: lease.WorkspaceID, RunID: lease.RunID, Generation: lease.AttemptGeneration,
			AttemptID: lease.AttemptID, GraphName: lease.GraphName, RunStartedAt: lease.RunStartedAt,
		},
		NextGeneration: req.NewAttemptGeneration, NextAttemptID: newAttemptID,
		LeaseTTL: ttl,
	})
	if err != nil || !updated || advanced.AttemptGeneration != req.NewAttemptGeneration || advanced.AttemptID != newAttemptID {
		if err == nil {
			err = errors.New("attempt advance CAS lost")
		}
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "advance parent attempt lease: %v", err)
	}
	if _, _, err := r.Fanout.CASAdmitResumeTx(ctx, tx, fanout.AdmitResumeRequest{
		WorkspaceID: req.WorkspaceID, GroupID: req.GroupID, ClaimID: req.ClaimID,
		PreviousAttemptGeneration: req.ExpectedAttemptGeneration, PreviousAttemptID: req.ExpectedAttemptID,
		NewAttemptGeneration: req.NewAttemptGeneration, NewAttemptID: req.NewAttemptID,
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *FanoutParentRunResumer) advanceGraph(ctx context.Context, req fanout.ResumeParkedRunRequest) (fanout.ResumeResult, error) {
	tx, err := r.Transactions.Begin(ctx)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	plan, err := r.Fanout.GetWorkflowResumePlanTx(ctx, tx, req.WorkspaceID, req.GroupID)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	if err := validateFanoutResumePlan(plan, req); err != nil {
		return fanout.ResumeResult{}, err
	}
	if plan.ResumeClaimState != nil && *plan.ResumeClaimState == fanout.ResumeClaimAdvanced {
		return fanout.ResumeResult{
			Status: fanout.ResumeAlreadyAdvanced, ClaimID: req.ClaimID,
			ResumeReceiptID: plan.ResumeReceiptID, AttemptGeneration: req.NewAttemptGeneration,
			AttemptID: req.NewAttemptID, ResumeReceipt: append(json.RawMessage(nil), plan.ResumeReceipt...),
		}, nil
	}
	if plan.ResumeClaimState == nil || *plan.ResumeClaimState != fanout.ResumeClaimAdmitted {
		return fanout.ResumeResult{Status: fanout.ResumeClaimConflict}, fanout.NewWorkflowError(fanout.ErrorResumeConflict, "resume claim is not admitted")
	}
	run, err := r.Runs.GetForUpdateTx(ctx, tx, req.WorkspaceID, req.ParentRunID)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	if run.Status != StatusRunning || int64(run.ExecutionLeaseEpoch) != req.NewAttemptGeneration || run.RunSnapshotID != req.RunSnapshotID {
		return fanout.ResumeResult{}, fanout.NewWorkflowError(fanout.ErrorResumeConflict, "admitted parent TeamRun identity differs")
	}
	checkpoint, err := r.Checkpoints.GetTx(ctx, tx, req.WorkspaceID, req.ParentRunID)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil {
		return fanout.ResumeResult{}, fanout.NewWorkflowError(fanout.ErrorResumeConflict, "admitted checkpoint differs: %v", err)
	}
	var join fanout.JoinResultV1
	if err := decodeFanoutExact(req.JoinResult, &join); err != nil || join.GroupID != req.GroupID ||
		join.GroupCompletionID != req.GroupCompletionID || join.Generation != req.Generation {
		return fanout.ResumeResult{}, fanout.NewWorkflowError(fanout.ErrorResumeConflict, "join result identity differs")
	}
	projection, err := fanout.ProjectJoinResult(join)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	extended, err := joinProjectionWithLegs(projection, join)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	encodedProjection, err := json.Marshal(extended)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	if checkpoint.CompletedOutputs == nil {
		checkpoint.CompletedOutputs = make(map[string]json.RawMessage)
	}
	checkpoint.CompletedOutputs[checkpoint.NodeID] = encodedProjection
	checkpoint.TeamRunGeneration = run.Generation
	checkpoint.ExecutionLeaseEpoch = run.ExecutionLeaseEpoch
	checkpoint.WrittenAt = r.now()
	if _, err := r.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
		return fanout.ResumeResult{}, err
	}
	taskID, err := fanout.DeriveResumeTaskID(req.WorkspaceID, req.ParentRunID, req.ClaimID)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	payload, _ := json.Marshal(map[string]any{
		"schema_version": 1, "kind": "fanout_resume", "parent_run_id": req.ParentRunID,
		"group_id": req.GroupID, "claim_id": req.ClaimID,
	})
	task := fanoutResumeTask(taskID, plan, req, payload)
	createdAt, existed, err := ensureFanoutResumeTaskTx(ctx, tx, r.Tasks, task)
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return fanout.ResumeResult{}, err
	}
	receiptID := "fre1_" + hex.EncodeToString(fanoutSHA256(req.WorkspaceID, req.ParentRunID, req.ClaimID))
	receipt, err := json.Marshal(fanoutResumeReceiptV1{
		WorkspaceID: req.WorkspaceID, ParentRunID: req.ParentRunID,
		GroupCompletionID: req.GroupCompletionID, ClaimID: req.ClaimID,
		PreviousAttemptGeneration: req.ExpectedAttemptGeneration,
		NewAttemptGeneration:      req.NewAttemptGeneration, NewAttemptID: req.NewAttemptID,
		ResumedCheckpointSequence: int64(run.ResumeGeneration), GraphAdvanceEvidenceID: taskID,
		AdvancedAt: createdAt.UTC(),
	})
	if err != nil {
		return fanout.ResumeResult{}, err
	}
	status := fanout.ResumeAdvanced
	if existed {
		status = fanout.ResumeAlreadyAdvanced
	}
	return fanout.ResumeResult{
		Status: status, ClaimID: req.ClaimID, ResumeReceiptID: receiptID,
		AttemptGeneration: req.NewAttemptGeneration, AttemptID: req.NewAttemptID,
		ResumeReceipt: receipt,
	}, nil
}

// joinProjectionWithLegs augments the fanout runtime projection with the
// machine-declared join legs shape so condition predicates can address
// per-branch results through machine-valid JSON pointers (for example
// /legs/0/result). The serial runtime keeps routing on
// schema_version/decision/results/errors and only carries the extra members.
func joinProjectionWithLegs(
	projection fanout.JoinProjectionV1,
	join fanout.JoinResultV1,
) (fanoutJoinProjectionV1, error) {
	raw, err := json.Marshal(projection)
	if err != nil {
		return fanoutJoinProjectionV1{}, err
	}
	var extended fanoutJoinProjectionV1
	if err := json.Unmarshal(raw, &extended); err != nil {
		return fanoutJoinProjectionV1{}, err
	}
	extended.Legs = make([]fanoutJoinLegV1, 0, len(join.Legs))
	for _, leg := range join.Legs {
		errorValue := json.RawMessage("null")
		if leg.ErrorCode != nil {
			errorValue, err = json.Marshal(*leg.ErrorCode)
			if err != nil {
				return fanoutJoinProjectionV1{}, err
			}
		}
		extended.Legs = append(extended.Legs, fanoutJoinLegV1{
			NodeID:              leg.BranchID,
			DecisionDisposition: string(leg.DecisionState),
			Result:              append(json.RawMessage(nil), leg.Result...),
			Error:               errorValue,
		})
	}
	return extended, nil
}

func fanoutResumeTask(
	taskID string,
	plan fanout.WorkflowResumePlan,
	req fanout.ResumeParkedRunRequest,
	payload json.RawMessage,
) *taskqueue.Task {
	return &taskqueue.Task{
		ID: taskID, WorkspaceID: req.WorkspaceID, IdentityKind: taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2, WorkflowID: plan.WorkflowID, WorkflowVersion: int(plan.WorkflowVersion),
		RunSnapshotID: req.RunSnapshotID, Source: "fanout_resume", Kind: "team_workflow",
		ContextKey: req.GroupID, Payload: payload,
	}
}

type fanoutResumeReceiptV1 struct {
	WorkspaceID               string    `json:"workspace_id"`
	ParentRunID               string    `json:"parent_run_id"`
	GroupCompletionID         string    `json:"group_completion_id"`
	ClaimID                   string    `json:"claim_id"`
	PreviousAttemptGeneration int64     `json:"previous_attempt_generation"`
	NewAttemptGeneration      int64     `json:"new_attempt_generation"`
	NewAttemptID              string    `json:"new_attempt_id"`
	ResumedCheckpointSequence int64     `json:"resumed_checkpoint_sequence"`
	GraphAdvanceEvidenceID    string    `json:"graph_advance_evidence_id"`
	AdvancedAt                time.Time `json:"advanced_at"`
}

func validateFanoutResumeRequest(req fanout.ResumeParkedRunRequest) error {
	if req.WorkspaceID == "" || req.ParentRunID == "" || req.RunSnapshotID == "" || req.IntentID == "" ||
		req.GroupID == "" || req.Generation == "" || req.ResumeToken == "" || req.GroupCompletionID == "" ||
		req.ClaimID == "" || req.ExpectedAttemptGeneration < 1 || req.ExpectedAttemptID == "" ||
		req.NewAttemptGeneration != req.ExpectedAttemptGeneration+1 || req.NewAttemptID == "" || !json.Valid(req.JoinResult) {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "resume request identity is invalid")
	}
	return nil
}

func validateFanoutResumePlan(plan fanout.WorkflowResumePlan, req fanout.ResumeParkedRunRequest) error {
	if plan.WorkspaceID != req.WorkspaceID || plan.GroupID != req.GroupID || plan.IntentID != req.IntentID ||
		plan.ParentRunID != req.ParentRunID || plan.RunSnapshotID != req.RunSnapshotID ||
		plan.Generation != req.Generation || plan.GroupCompletionID != req.GroupCompletionID ||
		plan.ResumeClaimID != req.ClaimID || plan.Mode != fanout.WorkflowResumeMode || !bytes.Equal(plan.JoinResult, req.JoinResult) {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "durable resume plan differs from request")
	}
	return nil
}

func validateFanoutParkedRun(run TeamRun, req fanout.ResumeParkedRunRequest) error {
	if run.Status != StatusParked || run.WaitKind == nil || *run.WaitKind != WaitFanout ||
		run.WorkspaceID != req.WorkspaceID || run.RunID != req.ParentRunID || run.RunSnapshotID != req.RunSnapshotID ||
		int64(run.ExecutionLeaseEpoch) != req.ExpectedAttemptGeneration {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "parked parent TeamRun identity differs")
	}
	var payload fanout.FanoutWaitPayload
	if err := decodeFanoutExact(run.WaitDetail, &payload); err != nil || payload.WaitType != "fanout_group" ||
		!payload.Parked || payload.IntentID != req.IntentID || payload.GroupID != req.GroupID ||
		payload.Generation != req.Generation || payload.ResumeToken != req.ResumeToken || payload.ParentRunID != req.ParentRunID {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "parked parent wait payload differs")
	}
	tokenHash := sha256.Sum256([]byte(req.ResumeToken))
	if !bytes.Equal(tokenHash[:], run.ResumeTokenHash) {
		return fanout.NewWorkflowError(fanout.ErrorResumeConflict, "parked parent token hash differs")
	}
	return nil
}

func ensureFanoutResumeTaskTx(ctx context.Context, tx pgx.Tx, tasks *taskqueue.Store, task *taskqueue.Task) (time.Time, bool, error) {
	var createdAt time.Time
	err := tx.QueryRow(ctx, `SELECT created_at FROM weave_task_queue
		WHERE workspace_id=$1 AND id=$2 AND identity_kind='team_workflow'
		  AND workflow_id=$3 AND workflow_version=$4 AND run_snapshot_id=$5
		  AND source='fanout_resume' AND kind='team_workflow'`,
		task.WorkspaceID, task.ID, task.WorkflowID, task.WorkflowVersion, task.RunSnapshotID).Scan(&createdAt)
	if err == nil {
		return createdAt, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, err
	}
	if err := tasks.EnqueueTx(ctx, tx, task); err != nil {
		return time.Time{}, false, err
	}
	if err := tx.QueryRow(ctx, `SELECT created_at FROM weave_task_queue WHERE workspace_id=$1 AND id=$2`,
		task.WorkspaceID, task.ID).Scan(&createdAt); err != nil {
		return time.Time{}, false, err
	}
	return createdAt, false, nil
}

func fanoutSHA256(fields ...string) []byte {
	hash := sha256.New()
	for _, field := range fields {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(field))
	}
	return hash.Sum(nil)
}

func decodeFanoutExact(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

type FanoutCoordinatorAdapter struct {
	Coordinator fanout.FanoutCoordinator
}

func (a FanoutCoordinatorAdapter) PreparePark(ctx context.Context, tx pgx.Tx, request FanoutPrepareRequest) (FanoutParkIntent, error) {
	var policy fanout.JoinPolicy
	if a.Coordinator == nil || decodeFanoutExact(request.JoinPolicy, &policy) != nil {
		return FanoutParkIntent{}, fanout.NewWorkflowError(fanout.ErrorStoreUnavailable, "fanout coordinator or policy is unavailable")
	}
	legs := make([]fanout.PlannedLeg, 0, len(request.Legs))
	for _, leg := range request.Legs {
		legs = append(legs, fanout.PlannedLeg{
			LegID: leg.LegID, BranchID: leg.BranchID, BranchOrdinal: leg.BranchOrdinal,
			FrozenBundleRef: leg.FrozenBundleRef, InputRef: leg.InputRef, MayYieldProof: leg.MayYieldProof,
		})
	}
	intent, err := a.Coordinator.PreparePark(ctx, tx, fanout.PrepareParkRequest{
		WorkspaceID: request.WorkspaceID, ParentRunID: request.ParentRunID,
		WorkflowID: request.WorkflowID, WorkflowVersion: request.WorkflowVersion,
		RunSnapshotID: request.RunSnapshotID, NodeID: request.NodeID,
		PreviousCheckpointSequence: request.PreviousCheckpointSequence, NodeEntryOrdinal: request.NodeEntryOrdinal,
		CreatorEpoch: request.CreatorEpoch, CreatorAttemptGeneration: request.CreatorAttemptGeneration,
		CreatorAttemptID: request.CreatorAttemptID, ActivationDeadline: request.ActivationDeadline,
		ResumeToken: request.ResumeToken, JoinPolicy: policy, Legs: legs,
	})
	if err != nil {
		return FanoutParkIntent{}, err
	}
	return FanoutParkIntent{IntentID: intent.IntentID, GroupID: intent.GroupID, Generation: intent.Generation}, nil
}

func (a FanoutCoordinatorAdapter) ActivatePark(ctx context.Context, request FanoutActivationRequest) error {
	_, err := a.Coordinator.ActivatePark(ctx, fanout.ActivateParkRequest{
		WorkspaceID: request.WorkspaceID, IntentID: request.IntentID, ParentRunID: request.ParentRunID,
		CheckpointSequence: request.CheckpointSequence, Generation: request.Generation, ResumeToken: request.ResumeToken,
	})
	return err
}

func (a FanoutCoordinatorAdapter) RecordLegCompletion(ctx context.Context, completion FanoutLegCompletion) error {
	_, err := a.Coordinator.RecordLegCompletion(ctx, fanout.LegCompletionRequest{
		WorkspaceID: completion.WorkspaceID, GroupID: completion.GroupID, LegID: completion.LegID,
		Generation: completion.Generation, Terminal: fanout.LegTerminal(completion.Terminal),
		Result: completion.Result, ErrorCode: completion.ErrorCode, CompletedAt: completion.CompletedAt,
	})
	return err
}

var _ fanout.CheckpointReader = (*FanoutCheckpointReader)(nil)
var _ fanout.CreatorLeaseReader = (*FanoutCreatorLeaseReader)(nil)
var _ fanout.ParentRunResumer = (*FanoutParentRunResumer)(nil)
var _ ExecutorFanout = FanoutCoordinatorAdapter{}
