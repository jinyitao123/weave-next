package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type CorrectionStatus string

const (
	CorrectionRequested CorrectionStatus = "requested"
	CorrectionReady     CorrectionStatus = "ready"
	CorrectionConfirmed CorrectionStatus = "confirmed"
	CorrectionDiscarded CorrectionStatus = "discarded"
	CorrectionApplied   CorrectionStatus = "applied"
)

type Correction struct {
	WorkspaceID                  string              `json:"workspace_id"`
	RunID                        string              `json:"run_id"`
	CorrectionID                 string              `json:"correction_id"`
	IdempotencyKey               string              `json:"-"`
	TargetKind                   string              `json:"target_kind"`
	TargetMemberID               string              `json:"target_member_id,omitempty"`
	Instruction                  string              `json:"instruction"`
	Status                       CorrectionStatus    `json:"status"`
	RequestedGeneration          TeamRunGeneration   `json:"requested_generation"`
	RequestedExecutionLeaseEpoch ExecutionLeaseEpoch `json:"requested_execution_lease_epoch"`
	SafeNodeID                   string              `json:"safe_node_id,omitempty"`
	RestartNodeID                string              `json:"restart_node_id,omitempty"`
	AffectedNodeIDs              []string            `json:"affected_node_ids,omitempty"`
	PreservedNodeIDs             []string            `json:"preserved_node_ids,omitempty"`
	RequestedBy                  string              `json:"requested_by"`
	RequestedAt                  time.Time           `json:"requested_at"`
	ReadyAt                      *time.Time          `json:"ready_at,omitempty"`
	ConfirmedBy                  string              `json:"confirmed_by,omitempty"`
	ConfirmedAt                  *time.Time          `json:"confirmed_at,omitempty"`
	AppliedAt                    *time.Time          `json:"applied_at,omitempty"`
}

type CorrectionDirectiveV1 struct {
	SchemaVersion  int      `json:"schema_version"`
	CorrectionID   string   `json:"correction_id"`
	TargetKind     string   `json:"target_kind"`
	TargetMemberID string   `json:"target_member_id,omitempty"`
	Instruction    string   `json:"instruction"`
	AffectedNodes  []string `json:"affected_node_ids"`
}

type CorrectionWaitDetailV1 struct {
	SchemaVersion    int      `json:"schema_version"`
	WaitType         string   `json:"wait_type"`
	CorrectionID     string   `json:"correction_id"`
	TargetKind       string   `json:"target_kind"`
	TargetMemberID   string   `json:"target_member_id,omitempty"`
	Instruction      string   `json:"instruction"`
	SafeNodeID       string   `json:"safe_node_id"`
	RestartNodeID    string   `json:"restart_node_id"`
	AffectedNodeIDs  []string `json:"affected_node_ids"`
	PreservedNodeIDs []string `json:"preserved_node_ids"`
}

func DecodeCorrectionWaitDetailV1(raw json.RawMessage) (CorrectionWaitDetailV1, error) {
	var detail CorrectionWaitDetailV1
	if len(raw) == 0 || len(raw) > ActivityDetailMaxBytes {
		return detail, errors.New("correction wait detail size is invalid")
	}
	if err := decodeExact(raw, &detail); err != nil {
		return detail, fmt.Errorf("decode correction wait detail: %w", err)
	}
	if detail.SchemaVersion != 1 || detail.WaitType != "correction" || detail.CorrectionID == "" ||
		detail.Instruction == "" || detail.SafeNodeID == "" || detail.RestartNodeID == "" ||
		(detail.TargetKind != "team" && detail.TargetKind != "member") ||
		(detail.TargetKind == "member" && detail.TargetMemberID == "") ||
		(detail.TargetKind == "team" && detail.TargetMemberID != "") || len(detail.AffectedNodeIDs) == 0 {
		return CorrectionWaitDetailV1{}, errors.New("correction wait detail fields are invalid")
	}
	return detail, nil
}

type RequestCorrectionRequest struct {
	WorkspaceID    string
	RunID          string
	TargetKind     string
	TargetMemberID string
	Instruction    string
	IdempotencyKey string
	Actor          string
	OccurredAt     time.Time
}

type CorrectionStore struct {
	Transactions TransactionBeginner
	Runs         *PGStore
}

func scanCorrection(row rowScanner) (Correction, error) {
	var item Correction
	var affected, preserved json.RawMessage
	var targetMember *string
	var safeNode, restartNode *string
	var confirmedBy *string
	var status string
	if err := row.Scan(&item.WorkspaceID, &item.RunID, &item.CorrectionID, &item.IdempotencyKey,
		&item.TargetKind, &targetMember, &item.Instruction, &status,
		&item.RequestedGeneration, &item.RequestedExecutionLeaseEpoch,
		&safeNode, &restartNode, &affected, &preserved, &item.RequestedBy, &item.RequestedAt,
		&item.ReadyAt, &confirmedBy, &item.ConfirmedAt, &item.AppliedAt); err != nil {
		return Correction{}, err
	}
	item.Status = CorrectionStatus(status)
	if targetMember != nil {
		item.TargetMemberID = *targetMember
	}
	if safeNode != nil {
		item.SafeNodeID = *safeNode
	}
	if restartNode != nil {
		item.RestartNodeID = *restartNode
	}
	if confirmedBy != nil {
		item.ConfirmedBy = *confirmedBy
	}
	if len(affected) != 0 {
		_ = json.Unmarshal(affected, &item.AffectedNodeIDs)
	}
	if len(preserved) != 0 {
		_ = json.Unmarshal(preserved, &item.PreservedNodeIDs)
	}
	return item, nil
}

const correctionColumns = `workspace_id,run_id,correction_id,idempotency_key,target_kind,target_member_id,
	instruction,status,requested_generation,requested_execution_lease_epoch,safe_node_id,restart_node_id,
	affected_node_ids,preserved_node_ids,requested_by,requested_at,ready_at,confirmed_by,confirmed_at,applied_at`

func (store *CorrectionStore) Request(ctx context.Context, req RequestCorrectionRequest) (Correction, error) {
	if store == nil || store.Transactions == nil || store.Runs == nil {
		return Correction{}, errors.New("team run correction store is unavailable")
	}
	req.Instruction = strings.TrimSpace(req.Instruction)
	if req.WorkspaceID == "" || req.RunID == "" || req.Actor == "" || req.IdempotencyKey == "" ||
		req.Instruction == "" || len(req.Instruction) > 16*1024 ||
		(req.TargetKind != "team" && req.TargetKind != "member") ||
		(req.TargetKind == "member" && req.TargetMemberID == "") ||
		(req.TargetKind == "team" && req.TargetMemberID != "") {
		return Correction{}, errors.New("team run correction request is invalid")
	}
	now := req.OccurredAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return Correction{}, fmt.Errorf("begin correction request: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if existing, present, readErr := readCorrectionByIdempotency(ctx, tx, req.WorkspaceID, req.RunID, req.IdempotencyKey); readErr != nil {
		return Correction{}, readErr
	} else if present {
		if existing.TargetKind != req.TargetKind || existing.TargetMemberID != req.TargetMemberID || existing.Instruction != req.Instruction {
			return Correction{}, ErrTeamRunStateConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return Correction{}, err
		}
		return existing, nil
	}
	run, err := store.Runs.GetForUpdateTx(ctx, tx, req.WorkspaceID, req.RunID)
	if err != nil {
		return Correction{}, err
	}
	correctable := run.Status == StatusRunning ||
		(run.Status == StatusParked && run.WaitKind != nil && *run.WaitKind == WaitFanout)
	if !correctable {
		return Correction{}, fmt.Errorf("%w: correction requires a running or fanout-waiting team run", ErrTeamRunStateConflict)
	}
	var activeCorrectionID string
	if activeErr := tx.QueryRow(ctx, `SELECT correction_id FROM weave_team_run_corrections
		WHERE workspace_id=$1 AND run_id=$2 AND status IN ('requested','ready','confirmed')
		LIMIT 1`, req.WorkspaceID, req.RunID).Scan(&activeCorrectionID); activeErr == nil {
		return Correction{}, fmt.Errorf("%w: correction %s is already active", ErrTeamRunStateConflict, activeCorrectionID)
	} else if !errors.Is(activeErr, pgx.ErrNoRows) {
		return Correction{}, fmt.Errorf("read active correction before request: %w", activeErr)
	}
	item := Correction{
		WorkspaceID: req.WorkspaceID, RunID: req.RunID, CorrectionID: uuid.NewString(),
		IdempotencyKey: req.IdempotencyKey, TargetKind: req.TargetKind,
		TargetMemberID: req.TargetMemberID, Instruction: req.Instruction, Status: CorrectionRequested,
		RequestedGeneration: run.Generation, RequestedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		RequestedBy: req.Actor, RequestedAt: now,
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_team_run_corrections (
		workspace_id,run_id,correction_id,idempotency_key,target_kind,target_member_id,instruction,status,
		requested_generation,requested_execution_lease_epoch,requested_by,requested_at
	) VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,$12)`,
		item.WorkspaceID, item.RunID, item.CorrectionID, item.IdempotencyKey, item.TargetKind,
		item.TargetMemberID, item.Instruction, item.Status, item.RequestedGeneration,
		item.RequestedExecutionLeaseEpoch, item.RequestedBy, item.RequestedAt)
	if err != nil {
		return Correction{}, fmt.Errorf("insert correction request: %w", err)
	}
	if err := appendCorrectionEvent(ctx, tx, item, "requested", req.Actor, json.RawMessage(`{}`), now); err != nil {
		return Correction{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Correction{}, fmt.Errorf("commit correction request: %w", err)
	}
	return item, nil
}

func readCorrectionByIdempotency(ctx context.Context, tx pgx.Tx, workspaceID, runID, key string) (Correction, bool, error) {
	item, err := scanCorrection(tx.QueryRow(ctx, `SELECT `+correctionColumns+` FROM weave_team_run_corrections
		WHERE workspace_id=$1 AND run_id=$2 AND idempotency_key=$3`, workspaceID, runID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Correction{}, false, nil
	}
	if err != nil {
		return Correction{}, false, fmt.Errorf("read correction by idempotency: %w", err)
	}
	return item, true, nil
}

func (store *CorrectionStore) GetActive(ctx context.Context, workspaceID, runID string) (Correction, bool, error) {
	if store == nil || store.Transactions == nil {
		return Correction{}, false, errors.New("team run correction store is unavailable")
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return Correction{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	item, err := scanCorrection(tx.QueryRow(ctx, `SELECT `+correctionColumns+` FROM weave_team_run_corrections
		WHERE workspace_id=$1 AND run_id=$2 AND status IN ('requested','ready','confirmed')
		ORDER BY requested_at DESC LIMIT 1`, workspaceID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		_ = tx.Commit(ctx)
		return Correction{}, false, nil
	}
	if err != nil {
		return Correction{}, false, fmt.Errorf("read active correction: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Correction{}, false, err
	}
	return item, true, nil
}

func (store *CorrectionStore) List(ctx context.Context, workspaceID, runID string, limit int) ([]Correction, error) {
	if store == nil || store.Transactions == nil {
		return nil, errors.New("team run correction store is unavailable")
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	tx, err := store.Transactions.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT `+correctionColumns+` FROM weave_team_run_corrections
		WHERE workspace_id=$1 AND run_id=$2 ORDER BY requested_at DESC,correction_id DESC LIMIT $3`, workspaceID, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Correction, 0)
	for rows.Next() {
		item, err := scanCorrection(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func (store *CorrectionStore) MarkReadyTx(ctx context.Context, tx pgx.Tx, workspaceID, runID string, detail CorrectionWaitDetailV1, actor string, occurredAt time.Time) (Correction, error) {
	if store == nil {
		return Correction{}, errors.New("team run correction store is unavailable")
	}
	affected, _ := json.Marshal(detail.AffectedNodeIDs)
	preserved, _ := json.Marshal(detail.PreservedNodeIDs)
	item, err := scanCorrection(tx.QueryRow(ctx, `UPDATE weave_team_run_corrections SET
		status='ready',safe_node_id=$4,restart_node_id=$5,affected_node_ids=$6::jsonb,
		preserved_node_ids=$7::jsonb,ready_at=$8
		WHERE workspace_id=$1 AND run_id=$2 AND correction_id=$3 AND status='requested'
		RETURNING `+correctionColumns, workspaceID, runID, detail.CorrectionID,
		detail.SafeNodeID, detail.RestartNodeID, string(affected), string(preserved), occurredAt.UTC()))
	if err != nil {
		return Correction{}, err
	}
	if err := appendCorrectionEvent(ctx, tx, item, "safe_point_reached", actor, mustJSON(map[string]any{
		"safe_node_id": detail.SafeNodeID, "restart_node_id": detail.RestartNodeID,
		"affected_node_ids": detail.AffectedNodeIDs, "preserved_node_ids": detail.PreservedNodeIDs,
	}), occurredAt); err != nil {
		return Correction{}, err
	}
	return item, nil
}

func appendCorrectionEvent(ctx context.Context, tx pgx.Tx, item Correction, kind, actor string, detail json.RawMessage, occurredAt time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO weave_team_run_correction_events
		(workspace_id,run_id,correction_id,event_kind,actor,detail,occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)`, item.WorkspaceID, item.RunID, item.CorrectionID,
		kind, actor, string(detail), occurredAt.UTC())
	if err != nil {
		return fmt.Errorf("append correction event: %w", err)
	}
	return nil
}

func mustJSON(value any) json.RawMessage { encoded, _ := json.Marshal(value); return encoded }

type CorrectionResumeTaskStore interface {
	EnqueueTx(context.Context, pgx.Tx, *taskqueue.Task) error
}

type ConfirmCorrectionRequest struct {
	WorkspaceID, RunID, CorrectionID, Disposition, IdempotencyKey, Actor string
	OccurredAt                                                           time.Time
}

type ConfirmCorrectionResult struct {
	Run        TeamRun
	TaskID     string
	Idempotent bool
}

type CorrectionResumeService struct {
	Transactions TransactionBeginner
	Runs         *PGStore
	Corrections  *CorrectionStore
	Checkpoints  *PGCheckpointStore
	Tasks        CorrectionResumeTaskStore
	Now          func() time.Time
}

func (s *CorrectionResumeService) Confirm(ctx context.Context, req ConfirmCorrectionRequest) (ConfirmCorrectionResult, error) {
	if s == nil || s.Transactions == nil || s.Runs == nil || s.Corrections == nil || s.Checkpoints == nil || s.Tasks == nil {
		return ConfirmCorrectionResult{}, errors.New("correction resume service is unavailable")
	}
	if req.WorkspaceID == "" || req.RunID == "" || req.CorrectionID == "" || req.IdempotencyKey == "" || req.Actor == "" ||
		(req.Disposition != "apply" && req.Disposition != "discard") {
		return ConfirmCorrectionResult{}, errors.New("correction confirmation is invalid")
	}
	now := req.OccurredAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
		if s.Now != nil {
			now = s.Now().UTC()
		}
	}
	taskID := correctionResumeTaskID(req.WorkspaceID, req.RunID, req.IdempotencyKey)
	tx, err := s.Transactions.Begin(ctx)
	if err != nil {
		return ConfirmCorrectionResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := s.Runs.GetForUpdateTx(ctx, tx, req.WorkspaceID, req.RunID)
	if err != nil {
		return ConfirmCorrectionResult{}, err
	}
	item, err := scanCorrection(tx.QueryRow(ctx, `SELECT `+correctionColumns+` FROM weave_team_run_corrections
		WHERE workspace_id=$1 AND run_id=$2 AND correction_id=$3 FOR UPDATE`, req.WorkspaceID, req.RunID, req.CorrectionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ConfirmCorrectionResult{}, ErrTeamRunIdentityMismatch
	}
	if err != nil {
		return ConfirmCorrectionResult{}, err
	}
	transition, transitionPresent, transitionErr := readTransitionByKey(ctx, tx, req.WorkspaceID, req.RunID, req.IdempotencyKey)
	if transitionErr != nil {
		return ConfirmCorrectionResult{}, transitionErr
	}
	expectedCorrectionStatus := CorrectionDiscarded
	if req.Disposition == "apply" {
		expectedCorrectionStatus = CorrectionConfirmed
		if item.Status == CorrectionApplied {
			expectedCorrectionStatus = CorrectionApplied
		}
	}
	if transitionPresent && transition.FromStatus != nil && *transition.FromStatus == StatusParked &&
		transition.ToStatus == StatusRunning && transition.Actor == req.Actor && transition.Source == "correction_resume" &&
		item.Status == expectedCorrectionStatus {
		if err := tx.Commit(ctx); err != nil {
			return ConfirmCorrectionResult{}, err
		}
		return ConfirmCorrectionResult{Run: run, TaskID: taskID, Idempotent: true}, nil
	}
	if run.Status != StatusParked || run.WaitKind == nil || *run.WaitKind != WaitCorrection || item.Status != CorrectionReady {
		return ConfirmCorrectionResult{}, ErrTeamRunStateConflict
	}
	detail, err := DecodeCorrectionWaitDetailV1(run.WaitDetail)
	if err != nil || detail.CorrectionID != item.CorrectionID {
		return ConfirmCorrectionResult{}, ErrTeamRunResumeInvalid
	}
	checkpoint, err := s.Checkpoints.GetTx(ctx, tx, run.WorkspaceID, run.RunID)
	if err != nil {
		return ConfirmCorrectionResult{}, err
	}
	if err := ValidateCheckpointRun(checkpoint, run); err != nil {
		return ConfirmCorrectionResult{}, err
	}
	newStatus := CorrectionDiscarded
	var confirmedBy any
	var confirmedAt any
	if req.Disposition == "apply" {
		newStatus = CorrectionConfirmed
		confirmedBy = req.Actor
		confirmedAt = now
	}
	updated, err := scanCorrection(tx.QueryRow(ctx, `UPDATE weave_team_run_corrections SET status=$4,confirmed_by=$5,confirmed_at=$6
		WHERE workspace_id=$1 AND run_id=$2 AND correction_id=$3 AND status='ready' RETURNING `+correctionColumns,
		req.WorkspaceID, req.RunID, req.CorrectionID, newStatus, confirmedBy, confirmedAt))
	if err != nil {
		return ConfirmCorrectionResult{}, err
	}
	if err := appendCorrectionEvent(ctx, tx, updated, req.Disposition, req.Actor, json.RawMessage(`{}`), now); err != nil {
		return ConfirmCorrectionResult{}, err
	}
	executorID := correctionResumeExecutorID(req.RunID, req.IdempotencyKey)
	resumed, err := s.Runs.ResumeRunningTx(ctx, tx, ResumeRequest{
		WorkspaceID: run.WorkspaceID, RunID: run.RunID, ExpectedStatus: StatusParked,
		ExpectedTeamRunGeneration: run.Generation, ExpectedExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		ExpectedResumeGeneration: run.ResumeGeneration, ExpectedWaitKind: WaitCorrection,
		ExpectedResumeTokenHash: run.ResumeTokenHash, ExecutorID: executorID,
		IdempotencyKey: req.IdempotencyKey, Actor: req.Actor, Source: "correction_resume", OccurredAt: now,
	})
	if err != nil {
		return ConfirmCorrectionResult{}, err
	}
	if req.Disposition == "apply" {
		checkpoint.NodeID = item.RestartNodeID
		for _, nodeID := range item.AffectedNodeIDs {
			delete(checkpoint.CompletedOutputs, nodeID)
			delete(checkpoint.DeliveryErrors, nodeID)
			delete(checkpoint.ArtifactTaskIDs, nodeID)
		}
		checkpoint.Corrections = append(checkpoint.Corrections, CorrectionDirectiveV1{
			SchemaVersion: 1, CorrectionID: item.CorrectionID, TargetKind: item.TargetKind,
			TargetMemberID: item.TargetMemberID, Instruction: item.Instruction,
			AffectedNodes: append([]string(nil), item.AffectedNodeIDs...),
		})
	}
	checkpoint.TeamRunGeneration = resumed.Generation
	checkpoint.ExecutionLeaseEpoch = resumed.ExecutionLeaseEpoch
	checkpoint.WrittenAt = now
	if _, err := s.Checkpoints.PutTx(ctx, tx, checkpoint); err != nil {
		return ConfirmCorrectionResult{}, err
	}
	payload, _ := json.Marshal(CorrectionResumeTaskPayloadV1{SchemaVersion: 1, Kind: "correction_resume", RunID: run.RunID,
		CorrectionID: item.CorrectionID, IdempotencyKey: req.IdempotencyKey, Disposition: req.Disposition})
	if err := s.Tasks.EnqueueTx(ctx, tx, &taskqueue.Task{ID: taskID, WorkspaceID: resumed.WorkspaceID,
		ProjectID: resumed.ProjectID, IdentityKind: taskqueue.IdentityTeamWorkflow, IdentitySchemaVersion: 2,
		WorkflowID: resumed.WorkflowID, WorkflowVersion: resumed.WorkflowVersion, RunSnapshotID: resumed.RunSnapshotID,
		Source: "correction_resume", Kind: "team_workflow", ContextKey: resumed.RunID, Payload: payload}); err != nil {
		return ConfirmCorrectionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ConfirmCorrectionResult{}, err
	}
	return ConfirmCorrectionResult{Run: resumed, TaskID: taskID}, nil
}

type CorrectionResumeTaskPayloadV1 struct {
	SchemaVersion  int    `json:"schema_version"`
	Kind           string `json:"kind"`
	RunID          string `json:"run_id"`
	CorrectionID   string `json:"correction_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Disposition    string `json:"disposition"`
}

func correctionResumeTaskID(workspaceID, runID, key string) string {
	d := sha256.Sum256([]byte("correction-resume-task:" + workspaceID + ":" + runID + ":" + key))
	return "crt1_" + hex.EncodeToString(d[:16])
}
func correctionResumeExecutorID(runID, key string) string {
	d := sha256.Sum256([]byte("correction-resume-executor:" + runID + ":" + key))
	return "teamrun-correction-resume:" + hex.EncodeToString(d[:16])
}

func (store *CorrectionStore) MarkAppliedTx(ctx context.Context, tx pgx.Tx, workspaceID, runID, correctionID, actor string, occurredAt time.Time) error {
	item, err := scanCorrection(tx.QueryRow(ctx, `UPDATE weave_team_run_corrections SET status='applied',applied_at=$4
		WHERE workspace_id=$1 AND run_id=$2 AND correction_id=$3 AND status='confirmed' RETURNING `+correctionColumns,
		workspaceID, runID, correctionID, occurredAt.UTC()))
	if errors.Is(err, pgx.ErrNoRows) {
		var alreadyApplied bool
		if readErr := tx.QueryRow(ctx, `SELECT status='applied' FROM weave_team_run_corrections
			WHERE workspace_id=$1 AND run_id=$2 AND correction_id=$3`, workspaceID, runID, correctionID).Scan(&alreadyApplied); readErr != nil {
			return readErr
		}
		if alreadyApplied {
			// Recovery can reclaim the same continuation after this commit.
			// Keep its original application time and single audit event so the
			// already-created engine attempt remains valid and reusable.
			return nil
		}
	}
	if err != nil {
		return err
	}
	return appendCorrectionEvent(ctx, tx, item, "applied", actor, json.RawMessage(`{}`), occurredAt)
}
