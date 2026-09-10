package deliverable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// GetVerificationReport reads an immutable current or historical report for an
// exact execution run. It cannot read a report from another workspace or run.
func (s *Store) GetVerificationReport(ctx context.Context, workspaceID, runID, verificationID string) (VerificationReport, error) {
	if s == nil || s.pool == nil {
		return VerificationReport{}, errors.New("delivery store unavailable")
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT report FROM weave_run_delivery_verifications WHERE workspace_id=$1 AND run_id=$2 AND verification_id=$3`, workspaceID, runID, verificationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return VerificationReport{}, ErrNotFound
	}
	if err != nil {
		return VerificationReport{}, err
	}
	var report VerificationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return VerificationReport{}, err
	}
	return report, nil
}

// RecheckCurrentDelivery observes an existing delivery without rerunning members
// or writing artifacts. Terminal executions, including cancelled runs, may be
// rechecked; this operation never changes their execution status or fence.
func (s *Store) RecheckCurrentDelivery(ctx context.Context, req RecheckRequest) (result RecheckResult, failure error) {
	defer func() {
		if failure != nil {
			result = RecheckResult{}
		}
	}()
	if s == nil || s.pool == nil {
		return result, errors.New("delivery store unavailable")
	}
	if req.WorkspaceID == "" || req.RunID == "" || req.RevisionID == "" || req.ContractDigest == "" {
		return result, ErrVerificationConflict
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	initial, err := s.GetDeliveryState(ctx, req.WorkspaceID, req.RunID)
	if err != nil {
		return result, err
	}
	if initial.Binding.RunID != req.RunID || initial.RevisionID != req.RevisionID || initial.ContractDigest != req.ContractDigest {
		return result, ErrVerificationConflict
	}
	if initial.Report == nil || initial.Report.OutputJSON == "" {
		return result, fmt.Errorf("%w: saved final JSON value is missing", ErrVerificationUnavailable)
	}
	candidate, err := s.restoreVerificationCandidate(ctx, *initial.Report)
	if err != nil {
		return result, err
	}
	started := time.Now().UTC()
	report, err := Verify(ctx, initial.Binding.Contract, candidate, s.verifiers)
	if err != nil {
		return result, err
	}
	if report.RevisionID != req.RevisionID || report.ContractDigest != req.ContractDigest {
		return result, ErrVerificationConflict
	}
	report.Fence = initial.Report.Fence
	report.Recheck = &RecheckObservation{StartedAt: started, CompletedAt: time.Now().UTC()}
	report.CreatedAt = report.Recheck.CompletedAt
	// A fresh observation has its own immutable evidence identity even when the
	// external state is unchanged. The delivery revision remains unchanged.
	identity, _ := json.Marshal(struct {
		ResultID    string
		Observation *RecheckObservation
	}{report.ID, report.Recheck})
	report.ID = "verification_" + digest(identity)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var snapshotID string
	if err := tx.QueryRow(ctx, `SELECT run_snapshot_id FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2 FOR UPDATE`, req.WorkspaceID, req.RunID).Scan(&snapshotID); err != nil {
		return result, err
	}
	if snapshotID != candidate.RunSnapshotID {
		return result, ErrVerificationConflict
	}
	current, err := readDeliveryState(ctx, tx, req.WorkspaceID, snapshotID, true)
	if err != nil {
		return result, err
	}
	if current.ContractDigest != req.ContractDigest || current.Binding.RunID != req.RunID {
		return result, ErrVerificationConflict
	}
	if err := validateCandidateSources(ctx, tx, candidate); err != nil {
		return result, err
	}
	persisted, err := insertVerificationReportTx(ctx, tx, report)
	if err != nil {
		return result, err
	}
	currentObservation := current.RevisionID == initial.RevisionID && current.SelectionSequence == initial.SelectionSequence && current.VerificationID == initial.VerificationID
	if currentObservation {
		tag, err := tx.Exec(ctx, `UPDATE weave_run_delivery_state SET current_verification_id=$4,selection_sequence=selection_sequence+1
			WHERE workspace_id=$1 AND run_snapshot_id=$2 AND current_revision_id=$3 AND contract_digest=$5 AND selection_sequence=$6`,
			req.WorkspaceID, snapshotID, req.RevisionID, report.ID, req.ContractDigest, initial.SelectionSequence)
		if err != nil {
			return result, err
		}
		if tag.RowsAffected() != 1 {
			return result, ErrVerificationConflict
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	return RecheckResult{Report: persisted, Current: currentObservation}, nil
}

func (s *Store) restoreVerificationCandidate(ctx context.Context, report VerificationReport) (Candidate, error) {
	var candidate Candidate
	raw, err := json.Marshal(report.Candidate)
	if err != nil {
		return candidate, err
	}
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return candidate, err
	}
	candidate.Output = []byte(report.OutputJSON)
	if !json.Valid(candidate.Output) || digest(candidate.Output) != candidate.OutputDigest {
		return candidate, fmt.Errorf("%w: saved final JSON value is invalid", ErrVerificationUnavailable)
	}
	filesBySource, err := loadCandidateSourceFiles(ctx, s.pool, candidate)
	if err != nil {
		return candidate, fmt.Errorf("%w: physical source cannot be reconstructed: %v", ErrVerificationUnavailable, err)
	}
	for index, artifact := range candidate.Artifacts {
		found := false
		for _, source := range artifact.Sources {
			for _, file := range filesBySource[source] {
				if file.Path == artifact.Path && file.ContentType == artifact.ContentType && digest([]byte(file.Content)) == artifact.SHA256 {
					candidate.Artifacts[index].Content = file.Content
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found && len(artifact.Sources) == 0 && validDigest(artifact.SHA256) {
			// Legacy outputs with no physical provenance remain unknown, but their
			// exact bytes may still be available in the immutable visible ledger.
			eventID := "workflow-final:" + candidate.RunID + ":" + candidate.NodeID + ":" + artifact.Path + ":" + artifact.SHA256[:24]
			err := s.pool.QueryRow(ctx, `SELECT content FROM weave_final_deliverables WHERE workspace_id=$1 AND run_id=$2 AND run_snapshot_id=$3 AND event_id=$4 AND content_type=$5`, candidate.WorkspaceID, candidate.RunID, candidate.RunSnapshotID, eventID, artifact.ContentType).Scan(&candidate.Artifacts[index].Content)
			if err == nil && digest([]byte(candidate.Artifacts[index].Content)) == artifact.SHA256 {
				found = true
			}
		}
		if !found {
			return candidate, fmt.Errorf("%w: saved file %q cannot be reconstructed", ErrVerificationUnavailable, artifact.Path)
		}
	}
	return candidate, nil
}

func insertVerificationReportTx(ctx context.Context, tx pgx.Tx, report VerificationReport) (VerificationReport, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return VerificationReport{}, err
	}
	candidate := report.Candidate
	_, err = tx.Exec(ctx, `INSERT INTO weave_run_delivery_verifications(workspace_id,run_id,run_snapshot_id,verification_id,revision_id,contract_digest,status,report,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9) ON CONFLICT (workspace_id,verification_id) DO NOTHING`, candidate.WorkspaceID, candidate.RunID, candidate.RunSnapshotID, report.ID, report.RevisionID, report.ContractDigest, report.Status, string(raw), report.CreatedAt)
	if err != nil {
		return VerificationReport{}, err
	}
	var persisted []byte
	if err := tx.QueryRow(ctx, `SELECT report FROM weave_run_delivery_verifications WHERE workspace_id=$1 AND verification_id=$2 AND run_id=$3 AND run_snapshot_id=$4 AND revision_id=$5 AND contract_digest=$6`, candidate.WorkspaceID, report.ID, candidate.RunID, candidate.RunSnapshotID, report.RevisionID, report.ContractDigest).Scan(&persisted); err != nil {
		return VerificationReport{}, ErrVerificationConflict
	}
	var saved VerificationReport
	if err := json.Unmarshal(persisted, &saved); err != nil {
		return VerificationReport{}, err
	}
	return saved, nil
}
