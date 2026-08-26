package teamrun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type PGStore struct{}

func NewPGStore() *PGStore { return &PGStore{} }

type rowScanner interface {
	Scan(...any) error
}

const teamRunColumns = `workspace_id,project_id,run_id,status,
	team_run_generation,execution_lease_epoch,resume_generation,
	team_id,workflow_id,workflow_version,run_snapshot_id,
	source_kind,source_task_id,establish_idempotency_key,
	current_executor_id,wait_kind,wait_detail,resume_token_hash,checkpoint_ref,
	cancel_actor,cancel_reason,cancel_idempotency_key,cancel_requested_at,
	cancel_grace_deadline_at,error_code,cause_summary,
	created_at,updated_at,terminal_at`

const transitionColumns = `workspace_id,run_id,seq,from_status,to_status,
	team_run_generation,execution_lease_epoch,resume_generation,
	actor,source,idempotency_key,payload_digest,error_code,cause_summary,occurred_at,orphaned`

func scanTeamRun(row rowScanner) (TeamRun, error) {
	var run TeamRun
	var status string
	var sourceKind string
	var waitKind *string
	var errorCode *string
	var projectID *string
	if err := row.Scan(
		&run.WorkspaceID,
		&projectID,
		&run.RunID,
		&status,
		&run.Generation,
		&run.ExecutionLeaseEpoch,
		&run.ResumeGeneration,
		&run.TeamID,
		&run.WorkflowID,
		&run.WorkflowVersion,
		&run.RunSnapshotID,
		&sourceKind,
		&run.SourceTaskID,
		&run.EstablishIdempotencyKey,
		&run.CurrentExecutorID,
		&waitKind,
		&run.WaitDetail,
		&run.ResumeTokenHash,
		&run.CheckpointRef,
		&run.CancelActor,
		&run.CancelReason,
		&run.CancelIdempotencyKey,
		&run.CancelRequestedAt,
		&run.CancelGraceDeadlineAt,
		&errorCode,
		&run.CauseSummary,
		&run.CreatedAt,
		&run.UpdatedAt,
		&run.TerminalAt,
	); err != nil {
		return TeamRun{}, err
	}
	if projectID != nil {
		run.ProjectID = *projectID
	}
	run.Status = Status(status)
	run.SourceKind = SourceKind(sourceKind)
	if waitKind != nil {
		value := WaitKind(*waitKind)
		run.WaitKind = &value
	}
	if errorCode != nil {
		value := ErrorCode(*errorCode)
		run.ErrorCode = &value
	}
	return run, nil
}

func scanTransition(row rowScanner) (Transition, error) {
	var transition Transition
	var fromStatus *string
	var toStatus string
	var errorCode *string
	if err := row.Scan(
		&transition.WorkspaceID,
		&transition.RunID,
		&transition.Seq,
		&fromStatus,
		&toStatus,
		&transition.Generation,
		&transition.ExecutionLeaseEpoch,
		&transition.ResumeGeneration,
		&transition.Actor,
		&transition.Source,
		&transition.IdempotencyKey,
		&transition.PayloadDigest,
		&errorCode,
		&transition.CauseSummary,
		&transition.OccurredAt,
		&transition.Orphaned,
	); err != nil {
		return Transition{}, err
	}
	if fromStatus != nil {
		value := Status(*fromStatus)
		transition.FromStatus = &value
	}
	transition.ToStatus = Status(toStatus)
	if errorCode != nil {
		value := ErrorCode(*errorCode)
		transition.ErrorCode = &value
	}
	return transition, nil
}

func requireTx(tx pgx.Tx) error {
	if tx == nil {
		return fmt.Errorf("tx must be non-nil")
	}
	return nil
}

func validatePhysicalKey(workspaceID, runID string) error {
	if workspaceID == "" || runID == "" {
		return fmt.Errorf("workspace_id and run_id must be non-empty")
	}
	return nil
}

type commandMeta struct {
	workspaceID        string
	runID              string
	expectedStatus     Status
	expectedGeneration TeamRunGeneration
	expectedEpoch      ExecutionLeaseEpoch
	expectedResume     ResumeGeneration
	idempotencyKey     string
	payloadDigest      []byte
	actor              string
	source             string
	occurredAt         time.Time
}

func validateCommand(meta commandMeta, allowed ...Status) error {
	if err := validatePhysicalKey(meta.workspaceID, meta.runID); err != nil {
		return err
	}
	if meta.expectedGeneration < 0 || meta.expectedEpoch < 0 || meta.expectedResume < 0 {
		return fmt.Errorf("expected generations and epochs must be non-negative")
	}
	allowedStatus := false
	for _, status := range allowed {
		if meta.expectedStatus == status {
			allowedStatus = true
			break
		}
	}
	if !allowedStatus {
		return fmt.Errorf("expected_status %q is invalid for command", meta.expectedStatus)
	}
	if meta.idempotencyKey == "" || meta.actor == "" || meta.source == "" {
		return fmt.Errorf("idempotency_key, actor, and source must be non-empty")
	}
	if meta.occurredAt.IsZero() {
		return fmt.Errorf("occurred_at must be non-zero")
	}
	return nil
}

func (store *PGStore) GetTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runID string,
) (TeamRun, error) {
	return store.get(ctx, tx, workspaceID, runID, false)
}

func (store *PGStore) GetForUpdateTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runID string,
) (TeamRun, error) {
	return store.get(ctx, tx, workspaceID, runID, true)
}

// ListCancelGraceExpiredTx locks a bounded batch of cancel requests whose
// grace deadline has elapsed. Callers must finish each transition and commit
// using the same transaction.
func (*PGStore) ListCancelGraceExpiredTx(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
	limit int,
) ([]TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, fmt.Errorf("now must be non-zero")
	}
	if limit < 1 {
		return nil, fmt.Errorf("limit must be positive")
	}
	rows, err := tx.Query(ctx, `SELECT `+teamRunColumns+`
		FROM weave_team_runs
		WHERE status='cancel_requested'
			AND cancel_grace_deadline_at<=$1
		ORDER BY cancel_grace_deadline_at,workspace_id,run_id
		FOR UPDATE SKIP LOCKED
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired team run cancel grace: %w", err)
	}
	defer rows.Close()

	runs := make([]TeamRun, 0)
	for rows.Next() {
		run, scanErr := scanTeamRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan expired team run cancel grace: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired team run cancel grace rows: %w", err)
	}
	return runs, nil
}

// ListTimerWakeExpiredTx locks a bounded batch of parked timer waits whose
// persisted wake_at has elapsed. The timestamp cast is the correctness path;
// the partial expression index is only an optional planner aid.
func (*PGStore) ListTimerWakeExpiredTx(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
	limit int,
) ([]TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, fmt.Errorf("now must be non-zero")
	}
	if limit < 1 {
		return nil, fmt.Errorf("limit must be positive")
	}
	rows, err := tx.Query(ctx, `SELECT `+teamRunColumns+`
		FROM weave_team_runs
		WHERE status='parked'
			AND wait_kind='timer'
			AND wait_detail ? 'wake_at'
			AND (wait_detail->>'wake_at')::timestamptz<=$1
		ORDER BY (wait_detail->>'wake_at')::timestamptz,workspace_id,run_id
		FOR UPDATE SKIP LOCKED
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired team run timer waits: %w", err)
	}
	defer rows.Close()

	runs := make([]TeamRun, 0)
	for rows.Next() {
		run, scanErr := scanTeamRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan expired team run timer wait: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired team run timer wait rows: %w", err)
	}
	return runs, nil
}

func (*PGStore) ListHumanDeadlineExpiredTx(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
	limit int,
) ([]TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	if now.IsZero() || limit < 1 {
		return nil, fmt.Errorf("now and a positive limit are required")
	}
	deadline := now.UTC().Truncate(time.Second).Format(time.RFC3339)
	rows, err := tx.Query(ctx, `SELECT `+teamRunColumns+`
		FROM weave_team_runs
		WHERE status='parked'
			AND wait_kind='human'
			AND wait_detail ? 'deadline_at'
			AND wait_detail->>'deadline_at'<=$1
		ORDER BY wait_detail->>'deadline_at',workspace_id,run_id
		FOR UPDATE SKIP LOCKED
		LIMIT $2`, deadline, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired team run human waits: %w", err)
	}
	defer rows.Close()
	runs := make([]TeamRun, 0)
	for rows.Next() {
		run, scanErr := scanTeamRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan expired team run human wait: %w", scanErr)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list expired team run human wait rows: %w", err)
	}
	return runs, nil
}

func (*PGStore) get(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runID string,
	forUpdate bool,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	if err := validatePhysicalKey(workspaceID, runID); err != nil {
		return TeamRun{}, err
	}
	query := `SELECT ` + teamRunColumns + `
		FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, query, workspaceID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamRun{}, fmt.Errorf("%w: team run not found", ErrTeamRunIdentityMismatch)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("read team run: %w", err)
	}
	return run, nil
}

func readTransitionByKey(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runID string,
	idempotencyKey string,
) (Transition, bool, error) {
	transition, err := scanTransition(tx.QueryRow(ctx, `SELECT `+transitionColumns+`
		FROM weave_team_run_transitions
		WHERE workspace_id=$1 AND run_id=$2 AND idempotency_key=$3`,
		workspaceID, runID, idempotencyKey,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Transition{}, false, nil
	}
	if err != nil {
		return Transition{}, false, fmt.Errorf("read team run transition: %w", err)
	}
	return transition, true, nil
}

func appendTransition(
	ctx context.Context,
	tx pgx.Tx,
	fromStatus *Status,
	run TeamRun,
	meta commandMeta,
	orphaned bool,
) error {
	var from any
	if fromStatus != nil {
		from = string(*fromStatus)
	}
	var errorCode any
	if run.ErrorCode != nil {
		errorCode = string(*run.ErrorCode)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO weave_team_run_transitions (
			workspace_id,run_id,seq,from_status,to_status,
			team_run_generation,execution_lease_epoch,resume_generation,
			actor,source,idempotency_key,payload_digest,error_code,cause_summary,occurred_at,orphaned
		)
		SELECT $1,$2,COALESCE(MAX(seq),0)+1,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15
		FROM weave_team_run_transitions
		WHERE workspace_id=$1 AND run_id=$2
		ON CONFLICT (workspace_id,run_id,idempotency_key) DO NOTHING`,
		run.WorkspaceID,
		run.RunID,
		from,
		string(run.Status),
		run.Generation,
		run.ExecutionLeaseEpoch,
		run.ResumeGeneration,
		meta.actor,
		meta.source,
		meta.idempotencyKey,
		meta.payloadDigest,
		errorCode,
		run.CauseSummary,
		meta.occurredAt,
		orphaned,
	)
	if err != nil {
		return fmt.Errorf("append team run transition: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: idempotency key already records a different transition", ErrTeamRunStateConflict)
	}
	return nil
}

func sameTransition(
	transition Transition,
	from Status,
	to Status,
	meta commandMeta,
	errorCode *ErrorCode,
	causeSummary *string,
) bool {
	if transition.Orphaned ||
		transition.FromStatus == nil ||
		*transition.FromStatus != from ||
		transition.ToStatus != to ||
		transition.Actor != meta.actor ||
		transition.Source != meta.source {
		return false
	}
	if !bytes.Equal(transition.PayloadDigest, meta.payloadDigest) {
		return false
	}
	if (transition.ErrorCode == nil) != (errorCode == nil) ||
		(transition.CauseSummary == nil) != (causeSummary == nil) {
		return false
	}
	if errorCode != nil && *transition.ErrorCode != *errorCode {
		return false
	}
	return causeSummary == nil || *transition.CauseSummary == *causeSummary
}

func classifyCAS(
	ctx context.Context,
	tx pgx.Tx,
	meta commandMeta,
	to Status,
	errorCode *ErrorCode,
	causeSummary *string,
	resumeCommand bool,
	recordOrphan bool,
) (TeamRun, error) {
	run, err := (&PGStore{}).GetForUpdateTx(ctx, tx, meta.workspaceID, meta.runID)
	if err != nil {
		return TeamRun{}, err
	}
	transition, present, err := readTransitionByKey(
		ctx, tx, meta.workspaceID, meta.runID, meta.idempotencyKey,
	)
	if err != nil {
		return TeamRun{}, err
	}
	if present {
		if sameTransition(transition, meta.expectedStatus, to, meta, errorCode, causeSummary) {
			return run, nil
		}
		return TeamRun{}, fmt.Errorf("%w: idempotency key facts differ", ErrTeamRunStateConflict)
	}
	stale := run.Generation != meta.expectedGeneration ||
		run.ExecutionLeaseEpoch != meta.expectedEpoch ||
		run.ResumeGeneration != meta.expectedResume
	if recordOrphan && run.ExecutionLeaseEpoch != meta.expectedEpoch {
		observed := run
		observed.Generation = meta.expectedGeneration
		observed.ExecutionLeaseEpoch = meta.expectedEpoch
		observed.ResumeGeneration = meta.expectedResume
		if err := appendTransition(
			ctx, tx, &run.Status, observed, meta, true,
		); err != nil {
			return TeamRun{}, err
		}
	}
	if run.Status == StatusCancelled {
		return TeamRun{}, ErrTeamRunCancelled
	}
	if run.Status == StatusAbandoned &&
		run.ErrorCode != nil &&
		*run.ErrorCode == ErrorCodeExecutionUnrecoverable {
		return TeamRun{}, ErrTeamRunExecutionUnrecoverable
	}
	if run.Status != meta.expectedStatus {
		return TeamRun{}, fmt.Errorf(
			"%w: expected %s, current %s",
			ErrTeamRunStateConflict,
			meta.expectedStatus,
			run.Status,
		)
	}
	if !stale {
		return TeamRun{}, ErrTeamRunStateConflict
	}
	if resumeCommand {
		return TeamRun{}, ErrTeamRunResumeStale
	}
	return TeamRun{}, ErrTeamRunStateConflict
}

func establishIdentityMatches(run TeamRun, req EstablishRequest) bool {
	return run.WorkspaceID == req.WorkspaceID &&
		run.ProjectID == req.ProjectID &&
		run.RunID == req.RunID &&
		run.TeamID == req.TeamID &&
		run.WorkflowID == req.WorkflowID &&
		run.WorkflowVersion == req.WorkflowVersion &&
		run.RunSnapshotID == req.RunSnapshotID &&
		run.SourceKind == req.SourceKind &&
		run.SourceTaskID == req.SourceTaskID &&
		run.EstablishIdempotencyKey == req.EstablishIdempotencyKey
}

func validateEstablishRequest(req EstablishRequest) error {
	if err := validatePhysicalKey(req.WorkspaceID, req.RunID); err != nil {
		return err
	}
	if req.TeamID == "" ||
		req.WorkflowID == "" ||
		req.WorkflowVersion < 1 ||
		req.RunSnapshotID == "" ||
		req.SourceTaskID == "" ||
		req.EstablishIdempotencyKey == "" ||
		req.Actor == "" ||
		req.Source == "" ||
		req.OccurredAt.IsZero() {
		return fmt.Errorf("establish identity, actor, source, and occurred_at must be valid")
	}
	switch req.SourceKind {
	case SourceSession, SourceSchedule, SourceManual, SourceAPI, SourceEvent:
		return nil
	default:
		return fmt.Errorf("source_kind %q is invalid", req.SourceKind)
	}
}

func (store *PGStore) EstablishQueuedTx(
	ctx context.Context,
	tx pgx.Tx,
	req EstablishRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	if err := validateEstablishRequest(req); err != nil {
		return TeamRun{}, err
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `INSERT INTO weave_team_runs (
			workspace_id,project_id,run_id,status,
			team_run_generation,execution_lease_epoch,resume_generation,
			team_id,workflow_id,workflow_version,run_snapshot_id,
			source_kind,source_task_id,establish_idempotency_key,
			created_at,updated_at
		) VALUES (
			$1,NULLIF($2,''),$3,'queued',0,0,0,$4,$5,$6,$7,$8,$9,$10,$11,$11
		)
		ON CONFLICT DO NOTHING
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.ProjectID,
		req.RunID,
		req.TeamID,
		req.WorkflowID,
		req.WorkflowVersion,
		req.RunSnapshotID,
		string(req.SourceKind),
		req.SourceTaskID,
		req.EstablishIdempotencyKey,
		req.OccurredAt,
	))
	if err == nil {
		meta := commandMeta{
			workspaceID:    req.WorkspaceID,
			runID:          req.RunID,
			idempotencyKey: req.EstablishIdempotencyKey,
			actor:          req.Actor,
			source:         req.Source,
			occurredAt:     req.OccurredAt,
		}
		if err := appendTransition(ctx, tx, nil, run, meta, false); err != nil {
			return TeamRun{}, err
		}
		return run, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TeamRun{}, fmt.Errorf("establish queued team run: %w", err)
	}
	existing, err := store.GetForUpdateTx(ctx, tx, req.WorkspaceID, req.RunID)
	if err != nil {
		// A source-task uniqueness collision can point at a different run_id.
		collision, collisionErr := scanTeamRun(tx.QueryRow(ctx, `SELECT `+teamRunColumns+`
			FROM weave_team_runs
			WHERE workspace_id=$1 AND source_kind=$2 AND source_task_id=$3
			FOR UPDATE`, req.WorkspaceID, string(req.SourceKind), req.SourceTaskID))
		if collisionErr == nil {
			existing = collision
		} else if !errors.Is(collisionErr, pgx.ErrNoRows) {
			return TeamRun{}, fmt.Errorf("read source task collision: %w", collisionErr)
		} else {
			return TeamRun{}, err
		}
	}
	if !establishIdentityMatches(existing, req) {
		return TeamRun{}, ErrTeamRunIdentityMismatch
	}
	return existing, nil
}

func claimMeta(req ClaimRequest) commandMeta {
	return commandMeta{
		workspaceID:        req.WorkspaceID,
		runID:              req.RunID,
		expectedStatus:     req.ExpectedStatus,
		expectedGeneration: req.ExpectedTeamRunGeneration,
		expectedEpoch:      req.ExpectedExecutionLeaseEpoch,
		expectedResume:     req.ExpectedResumeGeneration,
		idempotencyKey:     req.IdempotencyKey,
		actor:              req.Actor,
		source:             req.Source,
		occurredAt:         req.OccurredAt,
	}
}

func (store *PGStore) ClaimRunningTx(
	ctx context.Context,
	tx pgx.Tx,
	req ClaimRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := claimMeta(req)
	if err := validateCommand(meta, StatusQueued); err != nil {
		return TeamRun{}, err
	}
	if req.ExecutorID == "" {
		return TeamRun{}, fmt.Errorf("executor_id must be non-empty")
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='running',
			team_run_generation=team_run_generation+1,
			execution_lease_epoch=execution_lease_epoch+1,
			current_executor_id=$7,
			updated_at=$8
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND updated_at<=$8
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		req.ExecutorID,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusRunning, nil, nil, false, false)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("claim running team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func cancelQueuedMeta(req CancelQueuedRequest) commandMeta {
	return commandMeta{
		workspaceID:        req.WorkspaceID,
		runID:              req.RunID,
		expectedStatus:     req.ExpectedStatus,
		expectedGeneration: req.ExpectedTeamRunGeneration,
		expectedEpoch:      req.ExpectedExecutionLeaseEpoch,
		expectedResume:     req.ExpectedResumeGeneration,
		idempotencyKey:     req.IdempotencyKey,
		actor:              req.Actor,
		source:             req.Source,
		occurredAt:         req.OccurredAt,
	}
}

func (store *PGStore) CancelQueuedTx(
	ctx context.Context,
	tx pgx.Tx,
	req CancelQueuedRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := cancelQueuedMeta(req)
	if err := validateCommand(meta, StatusQueued); err != nil {
		return TeamRun{}, err
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='cancelled',
			team_run_generation=team_run_generation+1,
			error_code=$7,
			updated_at=$8,
			terminal_at=$8
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND updated_at<=$8
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		string(ErrorCodeCancelled),
		req.OccurredAt,
	))
	code := ErrorCodeCancelled
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusCancelled, &code, nil, false, false)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("cancel queued team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func parkMeta(req ParkRequest) commandMeta {
	return commandMeta{
		workspaceID:        req.WorkspaceID,
		runID:              req.RunID,
		expectedStatus:     req.ExpectedStatus,
		expectedGeneration: req.ExpectedTeamRunGeneration,
		expectedEpoch:      req.ExpectedExecutionLeaseEpoch,
		expectedResume:     req.ExpectedResumeGeneration,
		idempotencyKey:     req.IdempotencyKey,
		actor:              req.Actor,
		source:             req.Source,
		occurredAt:         req.OccurredAt,
	}
}

func normalizeWaitDetail(req ParkRequest) (json.RawMessage, error) {
	if len(req.WaitDetail) == 0 || !json.Valid(req.WaitDetail) {
		return nil, fmt.Errorf("wait_detail must be valid JSON")
	}
	var detail map[string]json.RawMessage
	if err := json.Unmarshal(req.WaitDetail, &detail); err != nil || detail == nil {
		return nil, fmt.Errorf("wait_detail must be a JSON object")
	}
	if req.WaitKind == WaitTimer {
		var timerDetail timerWaitDetail
		if err := decodeExact(req.WaitDetail, &timerDetail); err != nil ||
			timerDetail.WakeAt.IsZero() || timerDetail.NodeID == "" {
			return nil, fmt.Errorf("timer wait_detail requires only wake_at and node_id")
		}
		normalized, err := json.Marshal(timerDetail)
		if err != nil {
			return nil, fmt.Errorf("normalize timer wait_detail: %w", err)
		}
		return normalized, nil
	}
	if req.WaitKind == WaitHuman {
		if req.SessionLeaseEpoch != nil {
			return nil, fmt.Errorf("human wait forbids session_lease_epoch")
		}
		detail, err := DecodeHumanWaitDetailV1(req.WaitDetail)
		if err != nil {
			return nil, err
		}
		normalized, err := json.Marshal(detail)
		if err != nil {
			return nil, fmt.Errorf("normalize human wait_detail: %w", err)
		}
		return normalized, nil
	}
	delete(detail, "session_lease_epoch")
	if req.SessionLeaseEpoch != nil {
		return nil, fmt.Errorf("non-session wait forbids session_lease_epoch")
	}
	normalized, err := json.Marshal(detail)
	if err != nil {
		return nil, fmt.Errorf("normalize wait_detail: %w", err)
	}
	return normalized, nil
}

func (store *PGStore) ParkTx(
	ctx context.Context,
	tx pgx.Tx,
	req ParkRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := parkMeta(req)
	if err := validateCommand(meta, StatusRunning); err != nil {
		return TeamRun{}, err
	}
	if req.ExecutorID == "" ||
		req.CheckpointRef == "" ||
		len(req.ResumeTokenHash) == 0 {
		return TeamRun{}, fmt.Errorf("executor, checkpoint, and resume token hash must be non-empty")
	}
	switch req.WaitKind {
	case WaitTimer, WaitFanout, WaitHuman:
	default:
		return TeamRun{}, fmt.Errorf("wait_kind %q is invalid", req.WaitKind)
	}
	waitDetail, err := normalizeWaitDetail(req)
	if err != nil {
		return TeamRun{}, err
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='parked',
			team_run_generation=team_run_generation+1,
			resume_generation=resume_generation+1,
			current_executor_id=NULL,
			wait_kind=$8,
			wait_detail=$9::jsonb,
			resume_token_hash=$10,
			checkpoint_ref=$11,
			updated_at=$12
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND current_executor_id=$7
			AND updated_at<=$12
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		req.ExecutorID,
		string(req.WaitKind),
		waitDetail,
		req.ResumeTokenHash,
		req.CheckpointRef,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusParked, nil, nil, false, true)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("park team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func terminalMeta(
	workspaceID string,
	runID string,
	expectedStatus Status,
	expectedGeneration TeamRunGeneration,
	expectedEpoch ExecutionLeaseEpoch,
	expectedResume ResumeGeneration,
	idempotencyKey string,
	actor string,
	source string,
	occurredAt time.Time,
) commandMeta {
	return commandMeta{
		workspaceID:        workspaceID,
		runID:              runID,
		expectedStatus:     expectedStatus,
		expectedGeneration: expectedGeneration,
		expectedEpoch:      expectedEpoch,
		expectedResume:     expectedResume,
		idempotencyKey:     idempotencyKey,
		actor:              actor,
		source:             source,
		occurredAt:         occurredAt,
	}
}

func (store *PGStore) SucceedTx(
	ctx context.Context,
	tx pgx.Tx,
	req SucceedRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := terminalMeta(
		req.WorkspaceID, req.RunID, req.ExpectedStatus,
		req.ExpectedTeamRunGeneration, req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration, req.IdempotencyKey, req.Actor,
		req.Source, req.OccurredAt,
	)
	if err := validateCommand(meta, StatusRunning); err != nil {
		return TeamRun{}, err
	}
	if req.ExecutorID == "" {
		return TeamRun{}, fmt.Errorf("executor_id must be non-empty")
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='succeeded',
			team_run_generation=team_run_generation+1,
			current_executor_id=NULL,
			updated_at=$8,
			terminal_at=$8
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND current_executor_id=$7
			AND updated_at<=$8
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		req.ExecutorID,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusSucceeded, nil, nil, false, true)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("succeed team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func (store *PGStore) FailTx(
	ctx context.Context,
	tx pgx.Tx,
	req FailRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := terminalMeta(
		req.WorkspaceID, req.RunID, req.ExpectedStatus,
		req.ExpectedTeamRunGeneration, req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration, req.IdempotencyKey, req.Actor,
		req.Source, req.OccurredAt,
	)
	if err := validateCommand(meta, StatusRunning); err != nil {
		return TeamRun{}, err
	}
	if req.ExecutorID == "" || !ValidateErrorCode(req.ErrorCode) ||
		req.ErrorCode == ErrorCodeCancelled {
		return TeamRun{}, fmt.Errorf("executor_id and non-cancel error_code must be valid")
	}
	causeSummary := SanitizeCauseSummary(req.Cause)
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='failed',
			team_run_generation=team_run_generation+1,
			current_executor_id=NULL,
			error_code=$8,
			cause_summary=$9,
			updated_at=$10,
			terminal_at=$10
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND current_executor_id=$7
			AND updated_at<=$10
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		req.ExecutorID,
		string(req.ErrorCode),
		causeSummary,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(
			ctx, tx, meta, StatusFailed, &req.ErrorCode, causeSummary, false, true,
		)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("fail team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func requestCancelMeta(req RequestCancelRequest) commandMeta {
	return commandMeta{
		workspaceID:        req.WorkspaceID,
		runID:              req.RunID,
		expectedStatus:     req.ExpectedStatus,
		expectedGeneration: req.ExpectedTeamRunGeneration,
		expectedEpoch:      req.ExpectedExecutionLeaseEpoch,
		expectedResume:     req.ExpectedResumeGeneration,
		idempotencyKey:     req.IdempotencyKey,
		actor:              req.Actor,
		source:             req.Source,
		occurredAt:         req.OccurredAt,
	}
}

func (store *PGStore) RequestCancelTx(
	ctx context.Context,
	tx pgx.Tx,
	req RequestCancelRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := requestCancelMeta(req)
	if err := validateCommand(meta, StatusRunning, StatusParked); err != nil {
		return TeamRun{}, err
	}
	if req.CancelActor == "" ||
		req.CancelReason == "" ||
		req.GraceDeadlineAt.IsZero() ||
		req.GraceDeadlineAt.Before(req.OccurredAt) {
		return TeamRun{}, fmt.Errorf("cancel actor, reason, and grace deadline must be valid")
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='cancel_requested',
			team_run_generation=team_run_generation+1,
			resume_generation=CASE
				WHEN status='parked' THEN resume_generation+1
				ELSE resume_generation
			END,
			current_executor_id=NULL,
			wait_kind=NULL,
			wait_detail=NULL,
			resume_token_hash=NULL,
			checkpoint_ref=NULL,
			cancel_actor=$7,
			cancel_reason=$8,
			cancel_idempotency_key=$9,
			cancel_requested_at=$10,
			cancel_grace_deadline_at=$11,
			updated_at=$10
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND updated_at<=$10
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		req.CancelActor,
		req.CancelReason,
		req.IdempotencyKey,
		req.OccurredAt,
		req.GraceDeadlineAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusCancelRequested, nil, nil, false, true)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("request cancel team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func confirmCancelMeta(req ConfirmCancelRequest) commandMeta {
	return terminalMeta(
		req.WorkspaceID, req.RunID, req.ExpectedStatus,
		req.ExpectedTeamRunGeneration, req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration, req.IdempotencyKey, req.Actor,
		req.Source, req.OccurredAt,
	)
}

func (store *PGStore) ConfirmCancelTx(
	ctx context.Context,
	tx pgx.Tx,
	req ConfirmCancelRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := confirmCancelMeta(req)
	if err := validateCommand(meta, StatusCancelRequested); err != nil {
		return TeamRun{}, err
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='cancelled',
			team_run_generation=team_run_generation+1,
			cancel_actor=NULL,
			cancel_reason=NULL,
			cancel_idempotency_key=NULL,
			cancel_requested_at=NULL,
			cancel_grace_deadline_at=NULL,
			error_code=$7,
			updated_at=$8,
			terminal_at=$8
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND updated_at<=$8
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		string(ErrorCodeCancelled),
		req.OccurredAt,
	))
	code := ErrorCodeCancelled
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusCancelled, &code, nil, false, false)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("confirm cancel team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func abandonGraceMeta(req AbandonCancelGraceRequest) commandMeta {
	return terminalMeta(
		req.WorkspaceID, req.RunID, req.ExpectedStatus,
		req.ExpectedTeamRunGeneration, req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration, req.IdempotencyKey, req.Actor,
		req.Source, req.OccurredAt,
	)
}

func (store *PGStore) AbandonCancelGraceTx(
	ctx context.Context,
	tx pgx.Tx,
	req AbandonCancelGraceRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := abandonGraceMeta(req)
	if err := validateCommand(meta, StatusCancelRequested); err != nil {
		return TeamRun{}, err
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='abandoned',
			team_run_generation=team_run_generation+1,
			execution_lease_epoch=execution_lease_epoch+1,
			cancel_actor=NULL,
			cancel_reason=NULL,
			cancel_idempotency_key=NULL,
			cancel_requested_at=NULL,
			cancel_grace_deadline_at=NULL,
			error_code=$7,
			updated_at=$8,
			terminal_at=$8
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND cancel_grace_deadline_at<=$8
			AND updated_at<=$8
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		string(ErrorCodeExecutionUnrecoverable),
		req.OccurredAt,
	))
	code := ErrorCodeExecutionUnrecoverable
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusAbandoned, &code, nil, false, false)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("abandon cancel grace team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func reclaimMeta(req ReclaimRequest) commandMeta {
	return commandMeta{
		workspaceID:        req.WorkspaceID,
		runID:              req.RunID,
		expectedStatus:     req.ExpectedStatus,
		expectedGeneration: req.ExpectedTeamRunGeneration,
		expectedEpoch:      req.ExpectedExecutionLeaseEpoch,
		expectedResume:     req.ExpectedResumeGeneration,
		idempotencyKey:     req.IdempotencyKey,
		actor:              req.Actor,
		source:             req.Source,
		occurredAt:         req.OccurredAt,
	}
}

func (store *PGStore) ReclaimRunningTx(
	ctx context.Context,
	tx pgx.Tx,
	req ReclaimRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := reclaimMeta(req)
	if err := validateCommand(meta, StatusRunning); err != nil {
		return TeamRun{}, err
	}
	if req.ExecutorID == "" {
		return TeamRun{}, fmt.Errorf("executor_id must be non-empty")
	}
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			team_run_generation=team_run_generation+1,
			execution_lease_epoch=execution_lease_epoch+1,
			current_executor_id=$7,
			updated_at=$8
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND updated_at<=$8
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		req.ExecutorID,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusRunning, nil, nil, false, false)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("reclaim running team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func abandonUnrecoverableMeta(req AbandonUnrecoverableRequest) commandMeta {
	return terminalMeta(
		req.WorkspaceID, req.RunID, req.ExpectedStatus,
		req.ExpectedTeamRunGeneration, req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration, req.IdempotencyKey, req.Actor,
		req.Source, req.OccurredAt,
	)
}

func (store *PGStore) AbandonUnrecoverableTx(
	ctx context.Context,
	tx pgx.Tx,
	req AbandonUnrecoverableRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := abandonUnrecoverableMeta(req)
	if err := validateCommand(meta, StatusRunning); err != nil {
		return TeamRun{}, err
	}
	causeSummary := SanitizeCauseSummary(req.Cause)
	code := ErrorCodeExecutionUnrecoverable
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='abandoned',
			team_run_generation=team_run_generation+1,
			execution_lease_epoch=execution_lease_epoch+1,
			current_executor_id=NULL,
			error_code=$7,
			cause_summary=$8,
			updated_at=$9,
			terminal_at=$9
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND updated_at<=$9
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		string(code),
		causeSummary,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(
			ctx, tx, meta, StatusAbandoned, &code, causeSummary, false, false,
		)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("abandon unrecoverable team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func resumeMeta(req ResumeRequest) commandMeta {
	return commandMeta{
		workspaceID:        req.WorkspaceID,
		runID:              req.RunID,
		expectedStatus:     req.ExpectedStatus,
		expectedGeneration: req.ExpectedTeamRunGeneration,
		expectedEpoch:      req.ExpectedExecutionLeaseEpoch,
		expectedResume:     req.ExpectedResumeGeneration,
		idempotencyKey:     req.IdempotencyKey,
		payloadDigest:      append([]byte(nil), req.PayloadDigest...),
		actor:              req.Actor,
		source:             req.Source,
		occurredAt:         req.OccurredAt,
	}
}

func validateResumeEnvelope(run TeamRun, req ResumeRequest) error {
	if run.WaitKind == nil ||
		*run.WaitKind != req.ExpectedWaitKind ||
		!bytes.Equal(run.ResumeTokenHash, req.ExpectedResumeTokenHash) {
		return ErrTeamRunResumeInvalid
	}
	if req.ExpectedSessionLeaseEpoch != nil {
		return ErrTeamRunResumeInvalid
	}
	return nil
}

func (store *PGStore) ResumeRunningTx(
	ctx context.Context,
	tx pgx.Tx,
	req ResumeRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := resumeMeta(req)
	if err := validateCommand(meta, StatusParked); err != nil {
		return TeamRun{}, err
	}
	if req.ExecutorID == "" || len(req.ExpectedResumeTokenHash) == 0 {
		return TeamRun{}, fmt.Errorf("executor_id and resume token hash must be non-empty")
	}
	switch req.ExpectedWaitKind {
	case WaitTimer, WaitFanout:
		if req.HumanTimeout || len(req.Payload) != 0 || len(req.PayloadDigest) != 0 {
			return TeamRun{}, fmt.Errorf("payload is only allowed for human resume")
		}
	case WaitHuman:
		if req.HumanTimeout {
			if len(req.Payload) != 0 || len(req.PayloadDigest) != 0 {
				return TeamRun{}, fmt.Errorf("human timeout forbids payload")
			}
			break
		}
		if len(req.Payload) == 0 || !json.Valid(req.Payload) || len(req.Payload) > 64*1024 || len(req.PayloadDigest) != sha256.Size {
			return TeamRun{}, fmt.Errorf("human resume payload and digest are invalid")
		}
		digest := sha256.Sum256(req.Payload)
		if !bytes.Equal(digest[:], req.PayloadDigest) {
			return TeamRun{}, fmt.Errorf("human resume payload digest differs")
		}
	default:
		return TeamRun{}, fmt.Errorf("expected_wait_kind %q is invalid", req.ExpectedWaitKind)
	}

	locked, err := store.GetForUpdateTx(ctx, tx, req.WorkspaceID, req.RunID)
	if err != nil {
		return TeamRun{}, err
	}
	if locked.Status == StatusCancelled {
		return TeamRun{}, ErrTeamRunCancelled
	}
	if locked.Status != StatusParked {
		transition, present, readErr := readTransitionByKey(
			ctx, tx, req.WorkspaceID, req.RunID, req.IdempotencyKey,
		)
		if readErr != nil {
			return TeamRun{}, readErr
		}
		if present && sameTransition(
			transition, StatusParked, StatusRunning, meta, nil, nil,
		) {
			return locked, nil
		}
		return TeamRun{}, ErrTeamRunStateConflict
	}
	if locked.Generation != req.ExpectedTeamRunGeneration ||
		locked.ExecutionLeaseEpoch != req.ExpectedExecutionLeaseEpoch ||
		locked.ResumeGeneration != req.ExpectedResumeGeneration {
		return TeamRun{}, ErrTeamRunResumeStale
	}
	if err := validateResumeEnvelope(locked, req); err != nil {
		return TeamRun{}, err
	}

	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='running',
			team_run_generation=team_run_generation+1,
			execution_lease_epoch=execution_lease_epoch+CASE WHEN wait_kind='fanout' THEN 1 ELSE 0 END,
			current_executor_id=$10,
			wait_kind=NULL,
			wait_detail=NULL,
			resume_token_hash=NULL,
			checkpoint_ref=NULL,
			updated_at=$11
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND wait_kind=$7
			AND resume_token_hash=$8
			AND (
				($9::bigint IS NULL AND wait_kind<>'session')
				OR (wait_kind='session' AND (wait_detail->>'session_lease_epoch')::bigint=$9)
			)
			AND updated_at<=$11
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		string(req.ExpectedWaitKind),
		req.ExpectedResumeTokenHash,
		req.ExpectedSessionLeaseEpoch,
		req.ExecutorID,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(ctx, tx, meta, StatusRunning, nil, nil, true, false)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("resume running team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}

func failWaitTimeoutMeta(req FailWaitTimeoutRequest) commandMeta {
	return terminalMeta(
		req.WorkspaceID, req.RunID, req.ExpectedStatus,
		req.ExpectedTeamRunGeneration, req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration, req.IdempotencyKey, req.Actor,
		req.Source, req.OccurredAt,
	)
}

func (store *PGStore) FailWaitTimeoutTx(
	ctx context.Context,
	tx pgx.Tx,
	req FailWaitTimeoutRequest,
) (TeamRun, error) {
	if err := requireTx(tx); err != nil {
		return TeamRun{}, err
	}
	meta := failWaitTimeoutMeta(req)
	if err := validateCommand(meta, StatusParked); err != nil {
		return TeamRun{}, err
	}
	if !ValidateErrorCode(req.ErrorCode) || req.ErrorCode == ErrorCodeCancelled {
		return TeamRun{}, fmt.Errorf("non-cancel error_code must be valid")
	}
	switch req.ExpectedWaitKind {
	case WaitTimer, WaitFanout:
	default:
		return TeamRun{}, fmt.Errorf("expected_wait_kind %q is invalid", req.ExpectedWaitKind)
	}
	causeSummary := SanitizeCauseSummary(req.Cause)
	run, err := scanTeamRun(tx.QueryRow(ctx, `UPDATE weave_team_runs SET
			status='failed',
			team_run_generation=team_run_generation+1,
			wait_kind=NULL,
			wait_detail=NULL,
			resume_token_hash=NULL,
			checkpoint_ref=NULL,
			error_code=$8,
			cause_summary=$9,
			updated_at=$10,
			terminal_at=$10
		WHERE workspace_id=$1 AND run_id=$2 AND status=$3
			AND team_run_generation=$4
			AND execution_lease_epoch=$5
			AND resume_generation=$6
			AND wait_kind=$7
			AND updated_at<=$10
		RETURNING `+teamRunColumns,
		req.WorkspaceID,
		req.RunID,
		string(req.ExpectedStatus),
		req.ExpectedTeamRunGeneration,
		req.ExpectedExecutionLeaseEpoch,
		req.ExpectedResumeGeneration,
		string(req.ExpectedWaitKind),
		string(req.ErrorCode),
		causeSummary,
		req.OccurredAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return classifyCAS(
			ctx, tx, meta, StatusFailed, &req.ErrorCode, causeSummary, true, false,
		)
	}
	if err != nil {
		return TeamRun{}, fmt.Errorf("fail wait timeout team run: %w", err)
	}
	if err := appendTransition(ctx, tx, &req.ExpectedStatus, run, meta, false); err != nil {
		return TeamRun{}, err
	}
	return run, nil
}
