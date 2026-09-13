package fanout

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const workflowIntentColumns = `workspace_id,intent_id,parent_run_id,workflow_id,
	workflow_version,run_snapshot_id,node_id,previous_checkpoint_sequence,node_entry_ordinal,
	creator_epoch,creator_attempt_generation,creator_attempt_id::text,
	generation,plan_hash,status,checkpoint_sequence,activation_deadline_at,activated_at,
	voided_at,created_at,updated_at`

const workflowGroupColumns = `workspace_id,group_id,intent_id,mode,status,policy,generation,
	COALESCE(group_completion_id,''),COALESCE(decision,''),join_result,decided_at,
	COALESCE(resume_claim_id::text,''),resume_claim_state,created_at,updated_at`

func requireWorkflowTx(tx pgx.Tx) error {
	if tx == nil {
		return workflowError(ErrorInvalidRequest, "tx must be non-nil")
	}
	return nil
}

func scanParkIntent(row rowScanner) (ParkIntent, error) {
	var intent ParkIntent
	var status string
	if err := row.Scan(
		&intent.WorkspaceID, &intent.IntentID, &intent.ParentRunID, &intent.WorkflowID,
		&intent.WorkflowVersion, &intent.RunSnapshotID, &intent.NodeID,
		&intent.PreviousCheckpointSequence, &intent.NodeEntryOrdinal,
		&intent.CreatorEpoch, &intent.CreatorAttemptGeneration, &intent.CreatorAttemptID,
		&intent.Generation,
		&intent.PlanHash, &status, &intent.CheckpointSequence, &intent.ActivationDeadline,
		&intent.ActivatedAt, &intent.VoidedAt, &intent.CreatedAt, &intent.UpdatedAt,
	); err != nil {
		return ParkIntent{}, err
	}
	intent.Status = IntentStatus(status)
	return intent, nil
}

func scanWorkflowGroup(row rowScanner) (WorkflowGroup, error) {
	var group WorkflowGroup
	var mode, status string
	var policyRaw json.RawMessage
	var decision string
	var claimState *string
	if err := row.Scan(
		&group.WorkspaceID, &group.GroupID, &group.IntentID, &mode, &status, &policyRaw,
		&group.Generation, &group.GroupCompletionID, &decision, &group.JoinResult,
		&group.DecidedAt, &group.ResumeClaimID, &claimState, &group.CreatedAt, &group.UpdatedAt,
	); err != nil {
		return WorkflowGroup{}, err
	}
	if err := json.Unmarshal(policyRaw, &group.Policy); err != nil {
		return WorkflowGroup{}, fmt.Errorf("decode workflow group policy: %w", err)
	}
	group.Mode, group.Status, group.Decision = WorkflowGroupMode(mode), WorkflowGroupStatus(status), JoinDecision(decision)
	if claimState != nil {
		value := ResumeClaimState(*claimState)
		group.ResumeClaimState = &value
	}
	return group, nil
}

func (s *Store) CreateParkIntentTx(ctx context.Context, tx pgx.Tx, req PrepareParkRequest, modes ...WorkflowGroupMode) (ParkIntent, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return ParkIntent{}, err
	}
	mode := WorkflowResumeMode
	if len(modes) > 1 {
		return ParkIntent{}, workflowError(ErrorInvalidRequest, "at most one fanout mode is allowed")
	}
	if len(modes) == 1 {
		mode = modes[0]
	}
	if mode != WorkflowResumeMode && mode != FreeCollabSynthesisMode {
		return ParkIntent{}, workflowError(ErrorInvalidRequest, "unknown fanout mode %q", mode)
	}
	generation, err := DeriveGeneration(req.WorkspaceID, req.ParentRunID, req.NodeID,
		req.PreviousCheckpointSequence, req.NodeEntryOrdinal)
	if err != nil {
		return ParkIntent{}, err
	}
	planHash, err := DerivePlanHash(req)
	if err != nil {
		return ParkIntent{}, err
	}
	policy, err := canonicalJSON(normalizePolicy(req.JoinPolicy))
	if err != nil {
		return ParkIntent{}, workflowError(ErrorInvalidRequest, "%v", err)
	}
	now := s.clock.Now().UTC()
	intentID, groupID := "fi_"+uuid.NewString(), "fg_"+uuid.NewString()
	resumeTokenHash := sha256.Sum256([]byte(req.ResumeToken))
	created, err := scanParkIntent(tx.QueryRow(ctx, `
		INSERT INTO weave_fanout_intent (
			workspace_id,intent_id,parent_run_id,workflow_id,workflow_version,run_snapshot_id,
			node_id,previous_checkpoint_sequence,node_entry_ordinal,generation,plan_hash,
			resume_token_hash,creator_epoch,creator_attempt_generation,creator_attempt_id,
			activation_deadline_at,status,checkpoint_sequence,activated_at,voided_at,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'pending',NULL,NULL,NULL,$17,$17)
			ON CONFLICT DO NOTHING
		RETURNING `+workflowIntentColumns,
		req.WorkspaceID, intentID, req.ParentRunID, req.WorkflowID, req.WorkflowVersion,
		req.RunSnapshotID, req.NodeID, req.PreviousCheckpointSequence, req.NodeEntryOrdinal,
		generation, planHash, resumeTokenHash[:], req.CreatorEpoch, req.CreatorAttemptGeneration,
		req.CreatorAttemptID, req.ActivationDeadline.UTC(), now,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, readErr := s.getIntentByIdentityTx(ctx, tx, req.WorkspaceID, req.ParentRunID, req.NodeID, generation)
		if readErr != nil {
			return ParkIntent{}, readErr
		}
		if existing.PlanHash != planHash {
			return ParkIntent{}, workflowError(ErrorIntentConflict, "durable plan hash differs")
		}
		var sameResumeToken bool
		if err := tx.QueryRow(ctx, `SELECT resume_token_hash=$5 FROM weave_fanout_intent
			WHERE workspace_id=$1 AND parent_run_id=$2 AND node_id=$3 AND generation=$4`,
			req.WorkspaceID, req.ParentRunID, req.NodeID, generation, resumeTokenHash[:]).Scan(&sameResumeToken); err != nil {
			return ParkIntent{}, mapWorkflowStoreError("verify replay resume token", err)
		}
		if !sameResumeToken {
			return ParkIntent{}, workflowError(ErrorIntentConflict, "durable resume token differs")
		}
		existing.Reused = true
		return existing, nil
	}
	if err != nil {
		return ParkIntent{}, mapWorkflowStoreError("insert fanout intent", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_fanout_group (
			workspace_id,group_id,intent_id,mode,status,policy,generation,created_at,updated_at
		) VALUES ($1,$2,$3,$4,'pending_activation',$5,$6,$7,$7)
	`, req.WorkspaceID, groupID, intentID, string(mode), policy, generation, now); err != nil {
		return ParkIntent{}, mapWorkflowStoreError("insert fanout group", err)
	}
	for _, leg := range req.Legs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_fanout_leg (
				workspace_id,group_id,leg_id,branch_id,branch_ordinal,generation,
				frozen_bundle_ref,input_ref,may_yield_proof,status,created_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'pending_activation',$10,$10)
		`, req.WorkspaceID, groupID, leg.LegID, leg.BranchID, leg.BranchOrdinal, generation,
			leg.FrozenBundleRef, leg.InputRef, leg.MayYieldProof, now); err != nil {
			return ParkIntent{}, mapWorkflowStoreError("insert fanout leg", err)
		}
	}
	created.GroupID = groupID
	if err := s.AppendAuditTx(ctx, tx, AuditEvent{
		WorkspaceID: req.WorkspaceID, IntentID: intentID, GroupID: groupID,
		Generation: generation, EventType: "intent_created", Payload: json.RawMessage(`{}`), OccurredAt: now,
	}); err != nil {
		return ParkIntent{}, err
	}
	return created, nil
}

func (s *Store) GetIntent(ctx context.Context, workspaceID, intentID string) (ParkIntent, error) {
	intent, err := scanParkIntent(s.pool.QueryRow(ctx, `SELECT `+workflowIntentColumns+`
		FROM weave_fanout_intent WHERE workspace_id=$1 AND intent_id=$2`, workspaceID, intentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ParkIntent{}, workflowError(ErrorInvalidRequest, "fanout intent not found")
	}
	if err != nil {
		return ParkIntent{}, mapWorkflowStoreError("read fanout intent", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT group_id FROM weave_fanout_group
		WHERE workspace_id=$1 AND intent_id=$2`, workspaceID, intentID).Scan(&intent.GroupID); err != nil {
		return ParkIntent{}, mapWorkflowStoreError("read fanout intent group", err)
	}
	return intent, nil
}

func (s *Store) GetIntentTx(ctx context.Context, tx pgx.Tx, workspaceID, intentID string) (ParkIntent, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return ParkIntent{}, err
	}
	intent, err := scanParkIntent(tx.QueryRow(ctx, `SELECT `+workflowIntentColumns+`
		FROM weave_fanout_intent WHERE workspace_id=$1 AND intent_id=$2 FOR UPDATE`, workspaceID, intentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ParkIntent{}, workflowError(ErrorInvalidRequest, "intent not found")
	}
	if err != nil {
		return ParkIntent{}, mapWorkflowStoreError("read fanout intent for update", err)
	}
	return intent, nil
}

func (s *Store) GetIntentByIdentity(ctx context.Context, workspaceID, parentRunID, nodeID, generation string) (ParkIntent, error) {
	intent, err := scanParkIntent(s.pool.QueryRow(ctx, `SELECT `+workflowIntentColumns+`
		FROM weave_fanout_intent WHERE workspace_id=$1 AND parent_run_id=$2 AND node_id=$3 AND generation=$4`,
		workspaceID, parentRunID, nodeID, generation))
	if errors.Is(err, pgx.ErrNoRows) {
		return ParkIntent{}, workflowError(ErrorInvalidRequest, "fanout intent not found")
	}
	if err != nil {
		return ParkIntent{}, mapWorkflowStoreError("read fanout intent by identity", err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT group_id FROM weave_fanout_group
		WHERE workspace_id=$1 AND intent_id=$2`, workspaceID, intent.IntentID).Scan(&intent.GroupID); err != nil {
		return ParkIntent{}, mapWorkflowStoreError("read fanout intent group", err)
	}
	return intent, nil
}

func (s *Store) getIntentByIdentityTx(ctx context.Context, tx pgx.Tx, workspaceID, parentRunID, nodeID, generation string) (ParkIntent, error) {
	intent, err := scanParkIntent(tx.QueryRow(ctx, `SELECT `+workflowIntentColumns+`
			FROM weave_fanout_intent WHERE workspace_id=$1 AND parent_run_id=$2 AND node_id=$3 AND generation=$4`,
		workspaceID, parentRunID, nodeID, generation))
	if errors.Is(err, pgx.ErrNoRows) {
		return ParkIntent{}, workflowError(ErrorIntentConflict, "durable identity differs")
	}
	if err != nil {
		return ParkIntent{}, mapWorkflowStoreError("read existing fanout intent", err)
	}
	if err := tx.QueryRow(ctx, `SELECT group_id FROM weave_fanout_group
		WHERE workspace_id=$1 AND intent_id=$2`, workspaceID, intent.IntentID).Scan(&intent.GroupID); err != nil {
		return ParkIntent{}, mapWorkflowStoreError("read existing fanout group", err)
	}
	return intent, nil
}

func (s *Store) CASActivateIntentTx(ctx context.Context, tx pgx.Tx, workspaceID, intentID, generation string, checkpointSequence int64, activatedAt time.Time) (ActivationResult, error) {
	return s.casActivateIntentTx(ctx, tx, workspaceID, intentID, generation, checkpointSequence, activatedAt, IntentPending, ActivationActivated)
}

func (s *Store) CASReviveIntentTx(ctx context.Context, tx pgx.Tx, workspaceID, intentID, generation string, checkpointSequence int64, activatedAt time.Time) (ActivationResult, error) {
	return s.casActivateIntentTx(ctx, tx, workspaceID, intentID, generation, checkpointSequence, activatedAt, IntentVoided, ActivationRevived)
}

func (s *Store) casActivateIntentTx(ctx context.Context, tx pgx.Tx, workspaceID, intentID, generation string, checkpointSequence int64, activatedAt time.Time, from IntentStatus, disposition ActivationStatus) (ActivationResult, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return ActivationResult{}, err
	}
	if workspaceID == "" || intentID == "" || generation == "" || checkpointSequence < 0 || activatedAt.IsZero() {
		return ActivationResult{}, workflowError(ErrorInvalidRequest, "invalid activation request")
	}
	if err := validateZeroBasedJCS(checkpointSequence); err != nil {
		return ActivationResult{}, err
	}
	var groupID string
	err := tx.QueryRow(ctx, `
		UPDATE weave_fanout_intent
		SET status='active',checkpoint_sequence=$4,activated_at=$5,voided_at=NULL,updated_at=$5
		WHERE workspace_id=$1 AND intent_id=$2 AND generation=$3 AND status=$6
		RETURNING (SELECT group_id FROM weave_fanout_group
			WHERE workspace_id=$1 AND intent_id=$2)
	`, workspaceID, intentID, generation, checkpointSequence, activatedAt.UTC(), string(from)).Scan(&groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		if disposition == ActivationActivated {
			var status string
			var existingSequence *int64
			readErr := tx.QueryRow(ctx, `SELECT status,checkpoint_sequence FROM weave_fanout_intent
				WHERE workspace_id=$1 AND intent_id=$2 AND generation=$3`, workspaceID, intentID, generation).Scan(&status, &existingSequence)
			if readErr == nil && status == string(IntentActive) && existingSequence != nil && *existingSequence == checkpointSequence {
				if err := tx.QueryRow(ctx, `SELECT group_id FROM weave_fanout_group WHERE workspace_id=$1 AND intent_id=$2`, workspaceID, intentID).Scan(&groupID); err != nil {
					return ActivationResult{}, mapWorkflowStoreError("read active fanout group", err)
				}
				return ActivationResult{IntentID: intentID, GroupID: groupID, Status: ActivationAlreadyActive, Reused: true}, nil
			}
		}
		code := ErrorIntentNotRevivable
		if from == IntentPending {
			code = ErrorGenerationMismatch
		}
		return ActivationResult{}, workflowError(code, "intent activation CAS lost")
	}
	if err != nil {
		return ActivationResult{}, mapWorkflowStoreError("activate fanout intent", err)
	}
	groupTag, err := tx.Exec(ctx, `UPDATE weave_fanout_group SET status='active',updated_at=$4
		WHERE workspace_id=$1 AND group_id=$2 AND generation=$3 AND status='pending_activation'`,
		workspaceID, groupID, generation, activatedAt.UTC())
	if err != nil || groupTag.RowsAffected() != 1 {
		if err == nil {
			err = errors.New("group activation CAS lost")
		}
		return ActivationResult{}, mapWorkflowStoreError("activate fanout group", err)
	}
	legTag, err := tx.Exec(ctx, `UPDATE weave_fanout_leg SET status='queued',activated_at=$4,updated_at=$4
		WHERE workspace_id=$1 AND group_id=$2 AND generation=$3 AND status='pending_activation'`,
		workspaceID, groupID, generation, activatedAt.UTC())
	if err != nil || legTag.RowsAffected() == 0 {
		if err == nil {
			err = errors.New("no pending activation legs")
		}
		return ActivationResult{}, mapWorkflowStoreError("activate fanout legs", err)
	}
	var totalLegs int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM weave_fanout_leg
		WHERE workspace_id=$1 AND group_id=$2 AND generation=$3`, workspaceID, groupID, generation).Scan(&totalLegs); err != nil {
		return ActivationResult{}, mapWorkflowStoreError("count fanout activation legs", err)
	}
	if legTag.RowsAffected() != totalLegs {
		return ActivationResult{}, mapWorkflowStoreError("activate fanout legs", errors.New("not every planned leg was pending activation"))
	}
	if err := s.AppendAuditTx(ctx, tx, AuditEvent{
		WorkspaceID: workspaceID, IntentID: intentID, GroupID: groupID, Generation: generation,
		EventType: map[ActivationStatus]string{ActivationActivated: "intent_activated", ActivationRevived: "intent_revived"}[disposition],
		Payload:   json.RawMessage(`{}`), OccurredAt: activatedAt,
	}); err != nil {
		return ActivationResult{}, err
	}
	return ActivationResult{IntentID: intentID, GroupID: groupID, Status: disposition, Reused: disposition == ActivationRevived}, nil
}

func (s *Store) CASVoidIntentTx(ctx context.Context, tx pgx.Tx, workspaceID, intentID, generation string, voidedAt time.Time) (bool, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return false, err
	}
	var groupID string
	err := tx.QueryRow(ctx, `UPDATE weave_fanout_intent
		SET status='voided',activated_at=NULL,voided_at=$4,updated_at=$4
		WHERE workspace_id=$1 AND intent_id=$2 AND generation=$3 AND status='pending'
		RETURNING (SELECT group_id FROM weave_fanout_group WHERE workspace_id=$1 AND intent_id=$2)`,
		workspaceID, intentID, generation, voidedAt.UTC()).Scan(&groupID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, mapWorkflowStoreError("void fanout intent", err)
	}
	if err := s.AppendAuditTx(ctx, tx, AuditEvent{WorkspaceID: workspaceID, IntentID: intentID, GroupID: groupID,
		Generation: generation, EventType: "intent_voided", Payload: json.RawMessage(`{}`), OccurredAt: voidedAt}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) RecordLegTerminalTx(ctx context.Context, tx pgx.Tx, req LegCompletionRequest) (LegCompletionResult, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return LegCompletionResult{}, err
	}
	if req.WorkspaceID == "" || req.GroupID == "" || req.LegID == "" || req.Generation == "" || req.CompletedAt.IsZero() || !validTerminal(req.Terminal) {
		return LegCompletionResult{}, workflowError(ErrorInvalidRequest, "invalid leg completion")
	}
	if len(req.Result) != 0 && !json.Valid(req.Result) {
		return LegCompletionResult{}, workflowError(ErrorInvalidRequest, "leg result must be valid JSON")
	}
	var groupStatus, legStatus, durableGeneration string
	err := tx.QueryRow(ctx, `SELECT g.status,l.status,l.generation
		FROM weave_fanout_group g JOIN weave_fanout_leg l
		  ON l.workspace_id=g.workspace_id AND l.group_id=g.group_id
		WHERE g.workspace_id=$1 AND g.group_id=$2 AND l.leg_id=$3 FOR UPDATE OF g,l`,
		req.WorkspaceID, req.GroupID, req.LegID).Scan(&groupStatus, &legStatus, &durableGeneration)
	if err != nil {
		return LegCompletionResult{}, mapWorkflowStoreError("lock fanout leg completion", err)
	}
	eventType := "late_leg_completion"
	if durableGeneration != req.Generation {
		eventType = "stale_generation_write"
	} else if groupStatus == string(WorkflowGroupActive) && !isTerminalLegStatus(legStatus) {
		_, err := tx.Exec(ctx, `UPDATE weave_fanout_leg
			SET status=$4,result=$5,error_code=NULLIF($6,''),completed_at=$7,updated_at=$7
			WHERE workspace_id=$1 AND group_id=$2 AND leg_id=$3`,
			req.WorkspaceID, req.GroupID, req.LegID, string(req.Terminal), nullableJSON(req.Result), req.ErrorCode, req.CompletedAt.UTC())
		if err != nil {
			return LegCompletionResult{}, mapWorkflowStoreError("record fanout leg terminal", err)
		}
		return LegCompletionResult{Disposition: LegCompletionApplied}, nil
	}
	payload, _ := canonicalJSON(map[string]any{"terminal": req.Terminal, "error_code": req.ErrorCode})
	if err := s.AppendAuditTx(ctx, tx, AuditEvent{WorkspaceID: req.WorkspaceID, GroupID: req.GroupID,
		LegID: req.LegID, Generation: req.Generation, EventType: eventType, Payload: payload, OccurredAt: req.CompletedAt}); err != nil {
		return LegCompletionResult{}, err
	}
	return LegCompletionResult{Disposition: LegCompletionLateAuditOnly}, nil
}

func (s *Store) GroupLegSnapshot(ctx context.Context, tx pgx.Tx, workspaceID, groupID string) ([]GroupLegSnapshot, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT group_id,leg_id,branch_id,branch_ordinal,generation,status,result,COALESCE(error_code,''),completed_at
		FROM weave_fanout_leg WHERE workspace_id=$1 AND group_id=$2 ORDER BY branch_ordinal`, workspaceID, groupID)
	if err != nil {
		return nil, mapWorkflowStoreError("read fanout leg snapshot", err)
	}
	defer rows.Close()
	legs := make([]GroupLegSnapshot, 0)
	for rows.Next() {
		var leg GroupLegSnapshot
		var status string
		if err := rows.Scan(&leg.GroupID, &leg.LegID, &leg.BranchID, &leg.BranchOrdinal, &leg.Generation, &status, &leg.Result, &leg.ErrorCode, &leg.CompletedAt); err != nil {
			return nil, mapWorkflowStoreError("scan fanout leg snapshot", err)
		}
		leg.Status = LegDecisionState(status)
		legs = append(legs, leg)
	}
	if err := rows.Err(); err != nil {
		return nil, mapWorkflowStoreError("read fanout leg snapshot rows", err)
	}
	return legs, nil
}

func (s *Store) WorkflowLegPlansTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID string) ([]WorkflowLegPlan, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT workspace_id,group_id,leg_id,branch_id,branch_ordinal,
		generation,frozen_bundle_ref,input_ref,may_yield_proof
		FROM weave_fanout_leg WHERE workspace_id=$1 AND group_id=$2 ORDER BY branch_ordinal`, workspaceID, groupID)
	if err != nil {
		return nil, mapWorkflowStoreError("read workflow fanout leg 计划", err)
	}
	defer rows.Close()
	计划 := make([]WorkflowLegPlan, 0)
	for rows.Next() {
		var plan WorkflowLegPlan
		if err := rows.Scan(&plan.WorkspaceID, &plan.GroupID, &plan.LegID, &plan.BranchID,
			&plan.BranchOrdinal, &plan.Generation, &plan.FrozenBundleRef, &plan.InputRef,
			&plan.MayYieldProof); err != nil {
			return nil, mapWorkflowStoreError("scan workflow fanout leg plan", err)
		}
		计划 = append(计划, plan)
	}
	if err := rows.Err(); err != nil {
		return nil, mapWorkflowStoreError("read workflow fanout leg plan rows", err)
	}
	return 计划, nil
}

func (s *Store) CASCutQueuedLegsTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID, generation string, legIDs []string, now time.Time) (int64, error) {
	if len(legIDs) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_leg SET status='cut',completed_at=$5,updated_at=$5
		WHERE workspace_id=$1 AND group_id=$2 AND generation=$3 AND leg_id=ANY($4) AND status='queued'`,
		workspaceID, groupID, generation, legIDs, now.UTC())
	if err != nil {
		return 0, mapWorkflowStoreError("cut queued fanout legs", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) CASRequestCancelRunningLegsTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID, generation string, legIDs []string, graceDeadline, now time.Time) (int64, error) {
	if len(legIDs) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_leg
		SET status='cancel_requested',cancellation_grace_deadline_at=$5,updated_at=$6
		WHERE workspace_id=$1 AND group_id=$2 AND generation=$3 AND leg_id=ANY($4) AND status='running'`,
		workspaceID, groupID, generation, legIDs, graceDeadline.UTC(), now.UTC())
	if err != nil {
		return 0, mapWorkflowStoreError("request running fanout leg cancellation", err)
	}
	return tag.RowsAffected(), nil
}

func (s *Store) CASConfirmLegCancelledTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID, legID, generation string, completedAt time.Time) (bool, error) {
	return s.casFinishCancellationTx(ctx, tx, workspaceID, groupID, legID, generation, LegCancelled, completedAt)
}

func (s *Store) CASAbandonLegTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID, legID, generation string, completedAt time.Time) (bool, error) {
	return s.casFinishCancellationTx(ctx, tx, workspaceID, groupID, legID, generation, LegAbandoned, completedAt)
}

func (s *Store) casFinishCancellationTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID, legID, generation string, terminal LegTerminal, completedAt time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_leg
		SET status=$5,error_code=CASE WHEN $5='abandoned' THEN 'fanout_cancel_grace_expired' ELSE error_code END,
			completed_at=$6,updated_at=$6
		WHERE workspace_id=$1 AND group_id=$2 AND leg_id=$3 AND generation=$4 AND status='cancel_requested'`,
		workspaceID, groupID, legID, generation, string(terminal), completedAt.UTC())
	if err != nil {
		return false, mapWorkflowStoreError("finish fanout leg cancellation", err)
	}
	won := tag.RowsAffected() == 1
	if won {
		payload, _ := canonicalJSON(map[string]any{"terminal": terminal})
		if err := s.AppendAuditTx(ctx, tx, AuditEvent{
			WorkspaceID: workspaceID, GroupID: groupID, LegID: legID, Generation: generation,
			EventType: "leg_cancellation_terminal", Payload: payload, OccurredAt: completedAt,
		}); err != nil {
			return false, err
		}
	}
	return won, nil
}

func (s *Store) ListExpiredLegCancellationsTx(ctx context.Context, tx pgx.Tx, workspaceID string, now time.Time, limit int) ([]GroupLegSnapshot, error) {
	if limit < 1 || workspaceID == "" || now.IsZero() {
		return nil, workflowError(ErrorInvalidRequest, "invalid cancellation sweep")
	}
	rows, err := tx.Query(ctx, `SELECT group_id,leg_id,branch_id,branch_ordinal,generation,status,result,COALESCE(error_code,''),completed_at
		FROM weave_fanout_leg WHERE workspace_id=$1 AND status='cancel_requested'
		  AND cancellation_grace_deadline_at<=$2
		ORDER BY cancellation_grace_deadline_at,group_id,leg_id FOR UPDATE SKIP LOCKED LIMIT $3`, workspaceID, now.UTC(), limit)
	if err != nil {
		return nil, mapWorkflowStoreError("list expired fanout cancellations", err)
	}
	defer rows.Close()
	result := make([]GroupLegSnapshot, 0)
	for rows.Next() {
		var leg GroupLegSnapshot
		var status string
		if err := rows.Scan(&leg.GroupID, &leg.LegID, &leg.BranchID, &leg.BranchOrdinal, &leg.Generation, &status, &leg.Result, &leg.ErrorCode, &leg.CompletedAt); err != nil {
			return nil, mapWorkflowStoreError("scan expired fanout cancellation", err)
		}
		leg.Status = LegDecisionState(status)
		result = append(result, leg)
	}
	return result, rows.Err()
}

func (s *Store) TryDecideGroupTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID string, now time.Time, cancellationGrace time.Duration) (JoinEvaluation, bool, error) {
	if cancellationGrace < 0 {
		return JoinEvaluation{}, false, workflowError(ErrorInvalidRequest, "cancellation grace must be non-negative")
	}
	var status, generation, parentRunID, nodeID string
	var policyRaw json.RawMessage
	err := tx.QueryRow(ctx, `SELECT g.status,g.generation,g.policy,i.parent_run_id,i.node_id
		FROM weave_fanout_group g JOIN weave_fanout_intent i
		  ON i.workspace_id=g.workspace_id AND i.intent_id=g.intent_id
		WHERE g.workspace_id=$1 AND g.group_id=$2 FOR UPDATE OF g`, workspaceID, groupID).
		Scan(&status, &generation, &policyRaw, &parentRunID, &nodeID)
	if err != nil {
		return JoinEvaluation{}, false, mapWorkflowStoreError("lock fanout group decision", err)
	}
	if status != string(WorkflowGroupActive) {
		return JoinEvaluation{}, false, workflowError(ErrorGroupAlreadyDecided, "group status is %s", status)
	}
	var policy JoinPolicy
	if err := json.Unmarshal(policyRaw, &policy); err != nil {
		return JoinEvaluation{}, false, workflowError(ErrorStoreUnavailable, "decode join policy: %v", err)
	}
	legs, err := s.GroupLegSnapshot(ctx, tx, workspaceID, groupID)
	if err != nil {
		return JoinEvaluation{}, false, err
	}
	completionID, err := DeriveGroupCompletionID(workspaceID, parentRunID, nodeID, generation)
	if err != nil {
		return JoinEvaluation{}, false, err
	}
	evaluation, err := EvaluateJoin(groupID, completionID, generation, policy, legs, now)
	if err != nil || !evaluation.Ready {
		return evaluation, false, err
	}
	joinResult, err := canonicalJSON(evaluation.Frozen)
	if err != nil {
		return JoinEvaluation{}, false, mapWorkflowStoreError("encode frozen join result", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_group
		SET status='decided',group_completion_id=$3,decision=$4,join_result=$5,decided_at=$6,updated_at=$6
		WHERE workspace_id=$1 AND group_id=$2 AND status='active'`, workspaceID, groupID,
		completionID, string(evaluation.Decision), joinResult, now.UTC())
	if err != nil {
		return JoinEvaluation{}, false, mapWorkflowStoreError("decide fanout group", err)
	}
	if tag.RowsAffected() != 1 {
		return JoinEvaluation{}, false, workflowError(ErrorGroupAlreadyDecided, "group decision CAS lost")
	}
	if _, err := s.CASCutQueuedLegsTx(ctx, tx, workspaceID, groupID, generation, evaluation.Actions.CutLegIDs, now); err != nil {
		return JoinEvaluation{}, false, err
	}
	if _, err := s.CASRequestCancelRunningLegsTx(ctx, tx, workspaceID, groupID, generation,
		evaluation.Actions.RequestCancelLegIDs, now.Add(cancellationGrace), now); err != nil {
		return JoinEvaluation{}, false, err
	}
	if err := s.AppendAuditTx(ctx, tx, AuditEvent{WorkspaceID: workspaceID, GroupID: groupID,
		Generation: generation, EventType: "group_decided", Payload: json.RawMessage(`{}`), OccurredAt: now}); err != nil {
		return JoinEvaluation{}, false, err
	}
	return evaluation, true, nil
}

func (s *Store) CASClaimResumeTx(ctx context.Context, tx pgx.Tx, req ClaimResumeRequest) (ResumeClaim, bool, error) {
	if req.WorkspaceID == "" || req.GroupID == "" || req.GroupCompletionID == "" || req.ClaimID == "" ||
		req.PreviousAttemptGeneration < 0 || req.PreviousAttemptID == "" || req.ClaimedAt.IsZero() {
		return ResumeClaim{}, false, workflowError(ErrorInvalidRequest, "invalid resume claim")
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_group SET
		resume_claim_id=$4,resume_claim_state='claimed',
		resume_claim_previous_attempt_generation=$5,resume_claim_previous_attempt_id=$6,
		resume_claimed_group_completion_id=$3,resume_claimed_at=$7,updated_at=$7
		WHERE workspace_id=$1 AND group_id=$2 AND status='decided' AND mode='workflow_resume'
		  AND group_completion_id=$3 AND resume_claim_state IS NULL`, req.WorkspaceID, req.GroupID,
		req.GroupCompletionID, req.ClaimID, req.PreviousAttemptGeneration, req.PreviousAttemptID, req.ClaimedAt.UTC())
	if err != nil {
		return ResumeClaim{}, false, mapResumeStoreError("claim fanout resume", err)
	}
	claim, readErr := s.getResumeClaimTx(ctx, tx, req.WorkspaceID, req.GroupID)
	if readErr != nil {
		return ResumeClaim{}, false, readErr
	}
	if claim.ClaimID != req.ClaimID || claim.GroupCompletionID != req.GroupCompletionID ||
		claim.PreviousAttemptGeneration != req.PreviousAttemptGeneration || claim.PreviousAttemptID != req.PreviousAttemptID {
		return claim, false, workflowError(ErrorResumeConflict, "resume claim identity differs")
	}
	return claim, tag.RowsAffected() == 1, nil
}

func (s *Store) CASAdmitResumeTx(ctx context.Context, tx pgx.Tx, req AdmitResumeRequest) (ResumeClaim, bool, error) {
	if req.NewAttemptGeneration != req.PreviousAttemptGeneration+1 || req.NewAttemptID == "" {
		return ResumeClaim{}, false, workflowError(ErrorInvalidRequest, "new attempt must be previous generation plus one")
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_group SET
		resume_claim_state='admitted',resume_claim_new_attempt_generation=$6,
		resume_claim_new_attempt_id=$7,updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND group_id=$2 AND status='decided' AND resume_claim_state='claimed'
		  AND resume_claim_id=$3 AND resume_claim_previous_attempt_generation=$4
		  AND resume_claim_previous_attempt_id=$5 AND $6=$4+1`, req.WorkspaceID, req.GroupID,
		req.ClaimID, req.PreviousAttemptGeneration, req.PreviousAttemptID,
		req.NewAttemptGeneration, req.NewAttemptID)
	if err != nil {
		return ResumeClaim{}, false, mapWorkflowStoreError("admit fanout resume", err)
	}
	claim, readErr := s.getResumeClaimTx(ctx, tx, req.WorkspaceID, req.GroupID)
	if readErr != nil {
		return ResumeClaim{}, false, readErr
	}
	if claim.ClaimID != req.ClaimID || claim.NewAttemptGeneration == nil ||
		*claim.NewAttemptGeneration != req.NewAttemptGeneration || claim.NewAttemptID != req.NewAttemptID {
		return claim, false, workflowError(ErrorResumeConflict, "resume admission identity differs")
	}
	return claim, tag.RowsAffected() == 1, nil
}

func (s *Store) CASAdvanceResumeTx(ctx context.Context, tx pgx.Tx, req AdvanceResumeRequest) (ResumeClaim, bool, error) {
	receipt, err := validateResumeReceipt(req)
	if err != nil {
		return ResumeClaim{}, false, err
	}
	if req.ResumeReceiptID == "" || req.AdvancedAt.IsZero() {
		return ResumeClaim{}, false, workflowError(ErrorInvalidRequest, "invalid resume receipt")
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_group SET
		status='resumed',resume_claim_state='advanced',resume_receipt_id=$6,resume_receipt=$7,
		resume_advanced_at=$8,resumed_at=$8,updated_at=$8
		WHERE workspace_id=$1 AND group_id=$2 AND status='decided' AND resume_claim_state='admitted'
		  AND resume_claim_id=$3 AND resume_claim_new_attempt_generation=$4
		  AND resume_claim_new_attempt_id=$5 AND group_completion_id=$9
		  AND EXISTS (SELECT 1 FROM weave_fanout_intent i
		    WHERE i.workspace_id=weave_fanout_group.workspace_id
		      AND i.intent_id=weave_fanout_group.intent_id AND i.parent_run_id=$10)`,
		req.WorkspaceID, req.GroupID, req.ClaimID, req.NewAttemptGeneration, req.NewAttemptID,
		req.ResumeReceiptID, req.ResumeReceipt, req.AdvancedAt.UTC(), receipt.GroupCompletionID, receipt.ParentRunID)
	if err != nil {
		return ResumeClaim{}, false, mapResumeStoreError("advance fanout resume", err)
	}
	claim, readErr := s.getResumeClaimTx(ctx, tx, req.WorkspaceID, req.GroupID)
	if readErr != nil {
		return ResumeClaim{}, false, readErr
	}
	if claim.ClaimID != req.ClaimID || claim.State != ResumeClaimAdvanced ||
		claim.ResumeReceiptID != req.ResumeReceiptID {
		return claim, false, workflowError(ErrorResumeConflict, "resume advance identity differs")
	}
	return claim, tag.RowsAffected() == 1, nil
}

func (s *Store) CASCloseWorkflowResumeGroupTx(
	ctx context.Context,
	tx pgx.Tx,
	req CloseWorkflowResumeGroupRequest,
) (bool, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return false, err
	}
	if req.WorkspaceID == "" || req.GroupID == "" || req.Generation == "" ||
		req.GroupCompletionID == "" || req.Reason == "" || req.ClosedAt.IsZero() {
		return false, workflowError(ErrorInvalidRequest, "invalid workflow resume close request")
	}
	payload, err := canonicalJSON(map[string]any{"reason": req.Reason})
	if err != nil {
		return false, workflowError(ErrorInvalidRequest, "encode workflow resume close payload: %v", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_fanout_group
		SET status='closed',updated_at=$5
		WHERE workspace_id=$1 AND group_id=$2 AND status='decided' AND mode='workflow_resume'
		  AND generation=$3 AND group_completion_id=$4`, req.WorkspaceID, req.GroupID,
		req.Generation, req.GroupCompletionID, req.ClosedAt.UTC())
	if err != nil {
		return false, mapWorkflowStoreError("close workflow resume group", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil
	}
	if err := s.AppendAuditTx(ctx, tx, AuditEvent{
		WorkspaceID: req.WorkspaceID, GroupID: req.GroupID, Generation: req.Generation,
		EventType: "workflow_resume_closed", Payload: payload, OccurredAt: req.ClosedAt,
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) getResumeClaimTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID string) (ResumeClaim, error) {
	var claim ResumeClaim
	var state string
	err := tx.QueryRow(ctx, `SELECT workspace_id,group_id,group_completion_id,resume_claim_id::text,
		resume_claim_state,resume_claim_previous_attempt_generation,resume_claim_previous_attempt_id::text,
		resume_claim_new_attempt_generation,COALESCE(resume_claim_new_attempt_id::text,''),resume_claimed_at,
		COALESCE(resume_receipt_id,''),resume_receipt,resume_advanced_at
		FROM weave_fanout_group WHERE workspace_id=$1 AND group_id=$2 AND resume_claim_state IS NOT NULL`,
		workspaceID, groupID).Scan(&claim.WorkspaceID, &claim.GroupID, &claim.GroupCompletionID,
		&claim.ClaimID, &state, &claim.PreviousAttemptGeneration, &claim.PreviousAttemptID,
		&claim.NewAttemptGeneration, &claim.NewAttemptID, &claim.ClaimedAt, &claim.ResumeReceiptID,
		&claim.ResumeReceipt, &claim.AdvancedAt)
	if err != nil {
		return ResumeClaim{}, mapWorkflowStoreError("read fanout resume claim", err)
	}
	claim.State = ResumeClaimState(state)
	return claim, nil
}

func (s *Store) ListPendingIntentsForReconcileTx(ctx context.Context, tx pgx.Tx, workspaceID string, limit int) ([]ParkIntent, error) {
	if workspaceID == "" || limit < 1 {
		return nil, workflowError(ErrorInvalidRequest, "limit must be positive")
	}
	rows, err := tx.Query(ctx, `SELECT `+workflowIntentColumns+` FROM weave_fanout_intent
		WHERE workspace_id=$1 AND status='pending'
		ORDER BY activation_deadline_at,intent_id FOR UPDATE SKIP LOCKED LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, mapWorkflowStoreError("list pending fanout intents", err)
	}
	defer rows.Close()
	intents := make([]ParkIntent, 0)
	for rows.Next() {
		intent, err := scanParkIntent(rows)
		if err != nil {
			return nil, mapWorkflowStoreError("scan pending fanout intent", err)
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *Store) ListVoidedIntentsForReviveTx(ctx context.Context, tx pgx.Tx, workspaceID string, limit int) ([]ParkIntent, error) {
	if workspaceID == "" || limit < 1 {
		return nil, workflowError(ErrorInvalidRequest, "limit must be positive")
	}
	rows, err := tx.Query(ctx, `SELECT `+workflowIntentColumns+` FROM weave_fanout_intent
		WHERE workspace_id=$1 AND status='voided'
		ORDER BY updated_at,intent_id FOR UPDATE SKIP LOCKED LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, mapWorkflowStoreError("list voided fanout intents", err)
	}
	defer rows.Close()
	intents := make([]ParkIntent, 0)
	for rows.Next() {
		intent, err := scanParkIntent(rows)
		if err != nil {
			return nil, mapWorkflowStoreError("scan voided fanout intent", err)
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

func (s *Store) ListFanoutWorkspacesTx(ctx context.Context, tx pgx.Tx) ([]string, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT workspace_id FROM (
		SELECT workspace_id FROM weave_fanout_intent
		UNION SELECT workspace_id FROM weave_fanout_group
		UNION SELECT workspace_id FROM weave_fanout_leg
	) workspaces ORDER BY workspace_id`)
	if err != nil {
		return nil, mapWorkflowStoreError("list fanout workspaces", err)
	}
	defer rows.Close()
	workspaces := make([]string, 0)
	for rows.Next() {
		var workspaceID string
		if err := rows.Scan(&workspaceID); err != nil {
			return nil, mapWorkflowStoreError("scan fanout workspace", err)
		}
		workspaces = append(workspaces, workspaceID)
	}
	return workspaces, rows.Err()
}

func (s *Store) GetWorkflowResumePlanTx(ctx context.Context, tx pgx.Tx, workspaceID, groupID string) (WorkflowResumePlan, error) {
	if err := requireWorkflowTx(tx); err != nil {
		return WorkflowResumePlan{}, err
	}
	var plan WorkflowResumePlan
	var mode, status string
	var claimState *string
	err := tx.QueryRow(ctx, `SELECT i.workspace_id,i.intent_id,g.group_id,g.mode,g.status,
		i.parent_run_id,i.workflow_id,i.workflow_version,i.run_snapshot_id,i.node_id,i.generation,i.checkpoint_sequence,
		i.resume_token_hash,i.creator_epoch,i.creator_attempt_generation,i.creator_attempt_id::text,
		COALESCE(g.group_completion_id,''),g.join_result,COALESCE(g.resume_claim_id::text,''),
		g.resume_claim_state,COALESCE(g.resume_claim_previous_attempt_generation,0),
		COALESCE(g.resume_claim_previous_attempt_id::text,''),g.resume_claim_new_attempt_generation,
		COALESCE(g.resume_claim_new_attempt_id::text,''),COALESCE(g.resume_receipt_id,''),g.resume_receipt
		FROM weave_fanout_group g JOIN weave_fanout_intent i
		  ON i.workspace_id=g.workspace_id AND i.intent_id=g.intent_id
		WHERE g.workspace_id=$1 AND g.group_id=$2 FOR UPDATE OF g`, workspaceID, groupID).Scan(
		&plan.WorkspaceID, &plan.IntentID, &plan.GroupID, &mode, &status,
		&plan.ParentRunID, &plan.WorkflowID, &plan.WorkflowVersion, &plan.RunSnapshotID,
		&plan.NodeID, &plan.Generation, &plan.CheckpointSequence, &plan.ResumeTokenHash, &plan.CreatorEpoch,
		&plan.CreatorAttemptGeneration, &plan.CreatorAttemptID, &plan.GroupCompletionID,
		&plan.JoinResult, &plan.ResumeClaimID, &claimState, &plan.PreviousAttemptGeneration,
		&plan.PreviousAttemptID, &plan.NewAttemptGeneration, &plan.NewAttemptID,
		&plan.ResumeReceiptID, &plan.ResumeReceipt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowResumePlan{}, workflowError(ErrorInvalidRequest, "workflow fanout group not found")
	}
	if err != nil {
		return WorkflowResumePlan{}, mapWorkflowStoreError("read workflow resume plan", err)
	}
	plan.Mode, plan.Status = WorkflowGroupMode(mode), WorkflowGroupStatus(status)
	if claimState != nil {
		value := ResumeClaimState(*claimState)
		plan.ResumeClaimState = &value
	}
	return plan, nil
}

func (s *Store) ListGroupsForReconcileTx(ctx context.Context, tx pgx.Tx, workspaceID string, limit int) ([]WorkflowGroup, error) {
	if limit < 1 {
		return nil, workflowError(ErrorInvalidRequest, "limit must be positive")
	}
	rows, err := tx.Query(ctx, `SELECT `+workflowGroupColumns+` FROM weave_fanout_group
		WHERE workspace_id=$1 AND status IN ('active','decided')
		ORDER BY updated_at,group_id FOR UPDATE SKIP LOCKED LIMIT $2`, workspaceID, limit)
	if err != nil {
		return nil, mapWorkflowStoreError("list fanout groups for reconcile", err)
	}
	defer rows.Close()
	groups := make([]WorkflowGroup, 0)
	for rows.Next() {
		group, err := scanWorkflowGroup(rows)
		if err != nil {
			return nil, mapWorkflowStoreError("scan fanout reconcile group", err)
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) AppendAuditTx(ctx context.Context, tx pgx.Tx, event AuditEvent) error {
	if err := requireWorkflowTx(tx); err != nil {
		return err
	}
	if event.WorkspaceID == "" || event.Generation == "" || event.EventType == "" || event.OccurredAt.IsZero() || !isJSONObject(event.Payload) {
		return workflowError(ErrorInvalidRequest, "invalid fanout audit event")
	}
	if event.AuditID == "" {
		event.AuditID = "fa_" + uuid.NewString()
	}
	_, err := tx.Exec(ctx, `INSERT INTO weave_fanout_audit (
		workspace_id,audit_id,intent_id,group_id,leg_id,generation,event_type,payload,occurred_at,recorded_at
	) VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6,$7,$8,$9,$10)`,
		event.WorkspaceID, event.AuditID, event.IntentID, event.GroupID, event.LegID,
		event.Generation, event.EventType, event.Payload, event.OccurredAt.UTC(), s.clock.Now().UTC())
	if err != nil {
		return mapWorkflowStoreError("append fanout audit", err)
	}
	return nil
}

func validTerminal(terminal LegTerminal) bool {
	switch terminal {
	case LegSucceeded, LegFailed, LegTimeout, LegCut, LegCancelled, LegAbandoned:
		return true
	default:
		return false
	}
}

func isTerminalLegStatus(status string) bool {
	return validTerminal(LegTerminal(status))
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return raw
}

type resumeReceiptV1 struct {
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

func validateResumeReceipt(req AdvanceResumeRequest) (resumeReceiptV1, error) {
	if !isJSONObject(req.ResumeReceipt) {
		return resumeReceiptV1{}, workflowError(ErrorInvalidRequest, "resume receipt must be a JSON object")
	}
	var receipt resumeReceiptV1
	if err := json.Unmarshal(req.ResumeReceipt, &receipt); err != nil {
		return resumeReceiptV1{}, workflowError(ErrorInvalidRequest, "decode resume receipt: %v", err)
	}
	if receipt.WorkspaceID != req.WorkspaceID || receipt.ParentRunID == "" || receipt.GroupCompletionID == "" ||
		receipt.ClaimID != req.ClaimID || receipt.NewAttemptGeneration != req.NewAttemptGeneration ||
		receipt.NewAttemptID != req.NewAttemptID || receipt.GraphAdvanceEvidenceID == "" ||
		receipt.AdvancedAt.IsZero() || !receipt.AdvancedAt.Equal(req.AdvancedAt) {
		return resumeReceiptV1{}, workflowError(ErrorResumeConflict, "resume receipt identity differs")
	}
	if receipt.NewAttemptGeneration != receipt.PreviousAttemptGeneration+1 {
		return resumeReceiptV1{}, workflowError(ErrorResumeConflict, "resume receipt attempt generations are not adjacent")
	}
	if err := validateZeroBasedJCS(receipt.PreviousAttemptGeneration); err != nil {
		return resumeReceiptV1{}, err
	}
	if err := validatePositiveJCS(receipt.NewAttemptGeneration); err != nil {
		return resumeReceiptV1{}, err
	}
	if err := validateZeroBasedJCS(receipt.ResumedCheckpointSequence); err != nil {
		return resumeReceiptV1{}, err
	}
	return receipt, nil
}

func mapWorkflowStoreError(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return workflowError(ErrorIntentConflict, "%s: unique identity conflict", operation)
	}
	return workflowError(ErrorStoreUnavailable, "%s: %v", operation, err)
}

func mapResumeStoreError(operation string, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return workflowError(ErrorResumeConflict, "%s: unique resume identity conflict", operation)
	}
	return workflowError(ErrorStoreUnavailable, "%s: %v", operation, err)
}
