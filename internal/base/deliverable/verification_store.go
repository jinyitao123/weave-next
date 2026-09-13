package deliverable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
)

func NewWithVerifiers(pool *pgxpool.Pool, registry *VerifierRegistry, options ...StoreOption) *Store {
	s := New(pool, options...)
	s.verifiers = registry
	return s
}

type deliveryQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// FreezeContractTx belongs in the existing snapshot/enqueue transaction. No
// TeamRun is required yet. A replay must preserve every frozen binding fact.
func (s *Store) FreezeContractTx(ctx context.Context, tx pgx.Tx, req ContractBinding) (DeliveryState, error) {
	if s == nil || s.pool == nil || tx == nil {
		return DeliveryState{}, errors.New("delivery store unavailable")
	}
	if req.WorkspaceID == "" || req.RunSnapshotID == "" || req.WorkflowID == "" || req.WorkflowVersion < 1 {
		return DeliveryState{}, invalidVerification("contract binding incomplete")
	}
	cd, err := contractDigest(req.Contract)
	if err != nil {
		return DeliveryState{}, err
	}
	var workflowID, mode string
	var version int
	if err := tx.QueryRow(ctx, `SELECT workflow_id,workflow_version,mode FROM weave_team_run_snapshots WHERE workspace_id=$1 AND run_id=$2`, req.WorkspaceID, req.RunSnapshotID).Scan(&workflowID, &version, &mode); err != nil {
		return DeliveryState{}, fmt.Errorf("resolve delivery snapshot: %w", err)
	}
	if workflowID != req.WorkflowID || version != req.WorkflowVersion || mode != "fixed_workflow" {
		return DeliveryState{}, ErrVerificationConflict
	}
	raw, err := json.Marshal(req.Contract)
	if err != nil {
		return DeliveryState{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_run_delivery_state (workspace_id,run_snapshot_id,input_revision_id,workflow_id,workflow_version,published_digest,contract,contract_digest)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8) ON CONFLICT (workspace_id,run_snapshot_id) DO NOTHING`, req.WorkspaceID, req.RunSnapshotID, req.InputRevisionID, req.WorkflowID, req.WorkflowVersion, req.PublishedDigest, string(raw), cd)
	if err != nil {
		return DeliveryState{}, err
	}
	state, err := readDeliveryState(ctx, tx, req.WorkspaceID, req.RunSnapshotID, true)
	if err != nil {
		return state, err
	}
	b := state.Binding
	if state.ContractDigest != cd || b.InputRevisionID != req.InputRevisionID || b.WorkflowID != req.WorkflowID || b.WorkflowVersion != req.WorkflowVersion || b.PublishedDigest != req.PublishedDigest {
		return DeliveryState{}, ErrVerificationConflict
	}
	return state, nil
}

// GetDeliveryState accepts an exact execution run, or its dispatch snapshot
// before TeamRun exists. Workspace isolation applies to both forms.
func (s *Store) GetDeliveryState(ctx context.Context, workspaceID, runID string) (DeliveryState, error) {
	if s == nil || s.pool == nil {
		return DeliveryState{}, errors.New("delivery store unavailable")
	}
	snapshotID := runID
	err := s.pool.QueryRow(ctx, `SELECT run_snapshot_id FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`, workspaceID, runID).Scan(&snapshotID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return DeliveryState{}, err
	}
	state, err := readDeliveryState(ctx, s.pool, workspaceID, snapshotID, false)
	if err != nil {
		return state, err
	}
	if state.VerificationID != "" {
		var raw []byte
		if err := s.pool.QueryRow(ctx, `SELECT report FROM weave_run_delivery_verifications WHERE workspace_id=$1 AND verification_id=$2 AND run_snapshot_id=$3 AND revision_id=$4`, workspaceID, state.VerificationID, snapshotID, state.RevisionID).Scan(&raw); err != nil {
			return DeliveryState{}, err
		}
		var report VerificationReport
		if err := json.Unmarshal(raw, &report); err != nil {
			return DeliveryState{}, err
		}
		state.Report = &report
	}
	return state, nil
}

func readDeliveryState(ctx context.Context, query deliveryQuery, workspaceID, snapshotID string, lock bool) (DeliveryState, error) {
	sql := `SELECT workspace_id,COALESCE(run_id,''),run_snapshot_id,input_revision_id,workflow_id,workflow_version,published_digest,contract,contract_digest,COALESCE(current_revision_id,''),COALESCE(current_verification_id,''),selection_sequence FROM weave_run_delivery_state WHERE workspace_id=$1 AND run_snapshot_id=$2`
	if lock {
		sql += " FOR UPDATE"
	}
	var state DeliveryState
	var raw []byte
	b := &state.Binding
	err := query.QueryRow(ctx, sql, workspaceID, snapshotID).Scan(&b.WorkspaceID, &b.RunID, &b.RunSnapshotID, &b.InputRevisionID, &b.WorkflowID, &b.WorkflowVersion, &b.PublishedDigest, &raw, &state.ContractDigest, &state.RevisionID, &state.VerificationID, &state.SelectionSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, ErrNotFound
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &b.Contract); err != nil {
		return state, err
	}
	actual, err := contractDigest(b.Contract)
	if err != nil {
		return state, err
	}
	if actual != state.ContractDigest {
		return state, ErrVerificationConflict
	}
	return state, nil
}

// RecordVerifiedWorkflowOutputs runs read-only verification outside a database
// transaction, then commits the same actual outputs, immutable report and current
// pointer in the existing atomic bundle transaction. It never accepts a verdict.
func (s *Store) RecordVerifiedWorkflowOutputs(ctx context.Context, outputs []WorkflowOutput, fence VerificationFence) (saved VerificationReport, failure error) {
	defer func() {
		if failure != nil {
			saved = VerificationReport{}
		}
	}()
	if s == nil || s.pool == nil {
		return VerificationReport{}, errors.New("delivery store unavailable")
	}
	candidate, err := BuildCandidate(outputs)
	if err != nil {
		return VerificationReport{}, err
	}
	if candidate.WorkspaceID != fence.WorkspaceID || candidate.RunID != fence.RunID || candidate.RunSnapshotID != fence.RunSnapshotID || fence.ExecutorID == "" {
		return VerificationReport{}, ErrVerificationFence
	}
	state, err := readDeliveryState(ctx, s.pool, fence.WorkspaceID, fence.RunSnapshotID, false)
	missing := errors.Is(err, ErrNotFound)
	if err != nil && !missing {
		return VerificationReport{}, err
	}
	if missing {
		state.Binding = ContractBinding{WorkspaceID: fence.WorkspaceID, RunSnapshotID: fence.RunSnapshotID}
	}
	report, err := Verify(ctx, state.Binding.Contract, candidate, s.verifiers)
	if err != nil {
		return report, err
	}
	report.Fence = fence
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockVerificationRun(ctx, tx, fence); err != nil {
		return report, err
	}
	if missing {
		if err := tx.QueryRow(ctx, `SELECT workflow_id,workflow_version FROM weave_team_run_snapshots WHERE workspace_id=$1 AND run_id=$2`, fence.WorkspaceID, fence.RunSnapshotID).Scan(&state.Binding.WorkflowID, &state.Binding.WorkflowVersion); err != nil {
			return report, err
		}
		if _, err := s.FreezeContractTx(ctx, tx, state.Binding); err != nil {
			return report, err
		}
	}
	current, err := readDeliveryState(ctx, tx, fence.WorkspaceID, fence.RunSnapshotID, true)
	if err != nil {
		return report, err
	}
	if current.ContractDigest != report.ContractDigest || (current.Binding.RunID != "" && current.Binding.RunID != fence.RunID) {
		return report, ErrVerificationConflict
	}
	if current.SelectionSequence != state.SelectionSequence && (current.RevisionID != report.RevisionID || current.VerificationID != report.ID) {
		return report, ErrVerificationConflict
	}
	if err := validateCandidateSources(ctx, tx, candidate); err != nil {
		return report, err
	}
	if err := s.recordWorkflowOutputsTx(ctx, tx, outputs); err != nil {
		return report, err
	}
	persisted, err := insertVerificationReportTx(ctx, tx, report)
	if err != nil {
		return report, err
	}
	if current.RevisionID != report.RevisionID || current.VerificationID != report.ID {
		tag, err := tx.Exec(ctx, `UPDATE weave_run_delivery_state SET run_id=$3,current_revision_id=$4,current_verification_id=$5,selection_sequence=selection_sequence+1 WHERE workspace_id=$1 AND run_snapshot_id=$2 AND selection_sequence=$6`, fence.WorkspaceID, fence.RunSnapshotID, fence.RunID, report.RevisionID, report.ID, current.SelectionSequence)
		if err != nil {
			return report, err
		}
		if tag.RowsAffected() != 1 {
			return report, ErrVerificationConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	return persisted, nil
}

func lockVerificationRun(ctx context.Context, tx pgx.Tx, fence VerificationFence) error {
	var status, snapshotID, executor string
	var generation, lease, resume int64
	err := tx.QueryRow(ctx, `SELECT status,run_snapshot_id,team_run_generation,execution_lease_epoch,resume_generation,COALESCE(current_executor_id,'') FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2 FOR UPDATE`, fence.WorkspaceID, fence.RunID).Scan(&status, &snapshotID, &generation, &lease, &resume, &executor)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrVerificationFence
	}
	if err != nil {
		return err
	}
	if status != "running" || snapshotID != fence.RunSnapshotID || generation != fence.TeamRunGeneration || lease != fence.ExecutionLeaseEpoch || resume != fence.ResumeGeneration || executor != fence.ExecutorID {
		return ErrVerificationFence
	}
	return nil
}

func validateCandidateSources(ctx context.Context, tx pgx.Tx, candidate Candidate) error {
	_, err := loadCandidateSourceFiles(ctx, tx, candidate)
	return err
}

func loadCandidateSourceFiles(ctx context.Context, query deliveryQuery, candidate Candidate) (map[ArtifactSource][]fileartifact.File, error) {
	filesBySource := map[ArtifactSource][]fileartifact.File{}
	collectionsBySource := map[ArtifactSource]*fileartifact.CollectionEvidence{}
	for _, source := range candidate.Sources {
		if source.RunSnapshotID != candidate.RunSnapshotID || source.ParentRunID != candidate.RunID || !validDigest(source.ResultDigest) || (source.TaskID == "") == (source.MemberRunID == "") {
			return nil, ErrVerificationConflict
		}
		var raw []byte
		var files []fileartifact.File
		if source.TaskID != "" {
			var status, kind, snapshotID string
			if err := query.QueryRow(ctx, `SELECT result,status,kind,run_snapshot_id FROM weave_task_queue WHERE workspace_id=$1 AND id=$2 FOR SHARE`, candidate.WorkspaceID, source.TaskID).Scan(&raw, &status, &kind, &snapshotID); err != nil {
				return nil, fmt.Errorf("resolve file source: %w", err)
			}
			if status != "completed" || kind != "engine_exec" || snapshotID != candidate.RunSnapshotID {
				return nil, ErrVerificationConflict
			}
			var result struct {
				Artifacts          []fileartifact.File              `json:"artifacts"`
				ArtifactCollection *fileartifact.CollectionEvidence `json:"artifact_collection"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
			files = result.Artifacts
			if err := fileartifact.ValidateCollectionEvidence(result.ArtifactCollection); err != nil {
				return nil, err
			}
			collectionsBySource[source] = result.ArtifactCollection
		} else {
			if err := query.QueryRow(ctx, `SELECT result FROM weave_workflow_member_runs WHERE workspace_id=$1 AND parent_run_id=$2 AND run_snapshot_id=$3 AND member_run_id=$4 AND result IS NOT NULL FOR SHARE`, candidate.WorkspaceID, candidate.RunID, candidate.RunSnapshotID, source.MemberRunID).Scan(&raw); err != nil {
				return nil, fmt.Errorf("resolve member file source: %w", err)
			}
			var envelope struct {
				Result struct {
					RunID      string
					StopReason string
					State      map[string]json.RawMessage
				} `json:"result"`
				Error string `json:"error"`
			}
			if err := json.Unmarshal(raw, &envelope); err != nil {
				return nil, err
			}
			if envelope.Error != "" || envelope.Result.RunID != source.MemberRunID || envelope.Result.StopReason != "completed" {
				return nil, ErrVerificationConflict
			}
			if exported, ok := envelope.Result.State[fileartifact.MemberStateKey]; ok {
				if err := json.Unmarshal(exported, &files); err != nil {
					return nil, err
				}
			}
		}
		actual, err := CanonicalJSONDigest(raw)
		if err != nil {
			return nil, err
		}
		if actual != source.ResultDigest {
			return nil, ErrVerificationConflict
		}
		if err := fileartifact.Validate(files); err != nil {
			return nil, err
		}
		filesBySource[source] = files
	}
	for _, observation := range candidate.SourceObservations {
		if _, ok := filesBySource[observation.Source]; !ok {
			return nil, ErrVerificationConflict
		}
		expected, _ := json.Marshal(collectionsBySource[observation.Source])
		actual, _ := json.Marshal(observation.Collection)
		if string(expected) != string(actual) {
			return nil, ErrVerificationConflict
		}
	}
	for _, artifact := range candidate.Artifacts {
		for _, source := range artifact.Sources {
			found := false
			for _, file := range filesBySource[source] {
				if file.Path == artifact.Path && file.ContentType == artifact.ContentType && digest([]byte(file.Content)) == artifact.SHA256 {
					found = true
					break
				}
			}
			if !found {
				return nil, ErrVerificationConflict
			}
		}
	}
	return filesBySource, nil
}
