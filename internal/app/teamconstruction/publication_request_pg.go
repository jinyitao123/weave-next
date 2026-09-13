package teamconstruction

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/publication"
)

// Product transaction hooks are private to this server package. They may write
// the product's asset/build/usage-source tables only. No pgx type appears in the
// PublicationRequests or CandidateRequests boundaries consumed by the flows.
type pgPublicationRequests struct {
	pool           *pgxpool.Pool
	activate       func(context.Context, pgx.Tx, PublicationRequestRecord) error
	associateUsage func(context.Context, pgx.Tx, CandidateRequestRecord) error
}

func (s *pgPublicationRequests) ReservePublication(ctx context.Context, record PublicationRequestRecord) (PublicationRequestRecord, error) {
	if err := record.Verify(ctx, record.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	if record.State != PublicationPending {
		return PublicationRequestRecord{}, ErrPublicationTransition
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	subject, _ := json.Marshal(record.Subject)
	command, _ := json.Marshal(record.Command)
	_, err = tx.Exec(ctx, `INSERT INTO weave_team_publication_requests(workspace_id,request_id,actor_subject,request_digest,command,state) VALUES($1,$2,$3,$4,$5,'pending') ON CONFLICT(workspace_id,request_id) DO NOTHING`, record.Subject.WorkspaceID, record.Command.Request.RequestID, string(subject), record.Digest, string(command))
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	stored, err := loadPublicationRequest(ctx, tx, record)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PublicationRequestRecord{}, err
	}
	return stored, nil
}

func (s *pgPublicationRequests) RecordRevision(ctx context.Context, record PublicationRequestRecord) (PublicationRequestRecord, error) {
	if err := record.Verify(ctx, record.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	if record.Receipt == nil {
		return PublicationRequestRecord{}, publication.ErrInvalidReceipt
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := loadPublicationRequest(ctx, tx, record)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	next, err := stored.AcceptRevision(ctx, *record.Receipt)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if stored.Receipt == nil {
		encoded, _ := json.Marshal(next.Receipt)
		if _, err = tx.Exec(ctx, `UPDATE weave_team_publication_requests SET state='revision_obtained',receipt=$3,updated_at=now() WHERE workspace_id=$1 AND request_id=$2`, record.Subject.WorkspaceID, record.Command.Request.RequestID, string(encoded)); err != nil {
			return PublicationRequestRecord{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return PublicationRequestRecord{}, err
	}
	return next, nil
}

func (s *pgPublicationRequests) ActivatePublication(ctx context.Context, record PublicationRequestRecord) (PublicationRequestRecord, error) {
	if s.activate == nil {
		return PublicationRequestRecord{}, errors.New("product publication activation unavailable")
	}
	if err := record.Verify(ctx, record.Command); err != nil {
		return PublicationRequestRecord{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := loadPublicationRequest(ctx, tx, record)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if stored.State == PublicationActivated {
		return stored, nil
	}
	next, err := stored.Activated(ctx)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if err = s.activate(ctx, tx, stored); err != nil {
		return PublicationRequestRecord{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE weave_team_publication_requests SET state='activated',updated_at=now() WHERE workspace_id=$1 AND request_id=$2`, record.Subject.WorkspaceID, record.Command.Request.RequestID)
	if err != nil {
		return PublicationRequestRecord{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return PublicationRequestRecord{}, err
	}
	return next, nil
}

func loadPublicationRequest(ctx context.Context, tx pgx.Tx, expected PublicationRequestRecord) (PublicationRequestRecord, error) {
	var record PublicationRequestRecord
	var subject, command, receipt []byte
	err := tx.QueryRow(ctx, `SELECT actor_subject,request_digest,command,state,receipt FROM weave_team_publication_requests WHERE workspace_id=$1 AND request_id=$2 FOR UPDATE`, expected.Subject.WorkspaceID, expected.Command.Request.RequestID).Scan(&subject, &record.Digest, &command, &record.State, &receipt)
	if err != nil {
		return record, err
	}
	if err = json.Unmarshal(subject, &record.Subject); err != nil {
		return record, err
	}
	if record.Subject != expected.Subject {
		return PublicationRequestRecord{}, execution.ErrSubjectMismatch
	}
	if err = json.Unmarshal(command, &record.Command); err != nil {
		return record, err
	}
	if receipt != nil {
		if err = json.Unmarshal(receipt, &record.Receipt); err != nil {
			return record, err
		}
	}
	return record, record.Verify(ctx, expected.Command)
}

func (s *pgPublicationRequests) ReserveCandidate(ctx context.Context, record CandidateRequestRecord) (CandidateRequestRecord, error) {
	if err := record.Verify(ctx, record.Target, record.Request); err != nil {
		return CandidateRequestRecord{}, err
	}
	if record.Receipt != nil {
		return CandidateRequestRecord{}, publication.ErrInvalidReceipt
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	subject, _ := json.Marshal(record.Subject)
	target, _ := json.Marshal(record.Target)
	request, _ := json.Marshal(record.Request)
	_, err = tx.Exec(ctx, `INSERT INTO weave_team_candidate_requests(workspace_id,request_id,actor_subject,request_digest,target,request) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,request_id) DO NOTHING`, record.Subject.WorkspaceID, record.Request.RequestID, string(subject), record.Digest, string(target), string(request))
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	stored, err := loadCandidateRequest(ctx, tx, record)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CandidateRequestRecord{}, err
	}
	return stored, nil
}

func (s *pgPublicationRequests) RecordAdmission(ctx context.Context, record CandidateRequestRecord) (CandidateRequestRecord, error) {
	if s.associateUsage == nil {
		return CandidateRequestRecord{}, errors.New("candidate usage association unavailable")
	}
	if err := record.Verify(ctx, record.Target, record.Request); err != nil {
		return CandidateRequestRecord{}, err
	}
	if record.Receipt == nil {
		return CandidateRequestRecord{}, publication.ErrInvalidReceipt
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, err := loadCandidateRequest(ctx, tx, record)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if stored.Receipt != nil {
		if *stored.Receipt != *record.Receipt {
			return CandidateRequestRecord{}, publication.ErrRequestConflict
		}
		return stored, nil
	}
	stored.Receipt = record.Receipt
	if err = s.associateUsage(ctx, tx, stored); err != nil {
		return CandidateRequestRecord{}, err
	}
	encoded, _ := json.Marshal(stored.Receipt)
	_, err = tx.Exec(ctx, `UPDATE weave_team_candidate_requests SET receipt=$3,updated_at=now() WHERE workspace_id=$1 AND request_id=$2`, record.Subject.WorkspaceID, record.Request.RequestID, string(encoded))
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return CandidateRequestRecord{}, err
	}
	return stored, nil
}

func loadCandidateRequest(ctx context.Context, tx pgx.Tx, expected CandidateRequestRecord) (CandidateRequestRecord, error) {
	var record CandidateRequestRecord
	var subject, target, request, receipt []byte
	err := tx.QueryRow(ctx, `SELECT actor_subject,request_digest,target,request,receipt FROM weave_team_candidate_requests WHERE workspace_id=$1 AND request_id=$2 FOR UPDATE`, expected.Subject.WorkspaceID, expected.Request.RequestID).Scan(&subject, &record.Digest, &target, &request, &receipt)
	if err != nil {
		return record, err
	}
	if err = json.Unmarshal(subject, &record.Subject); err != nil {
		return record, err
	}
	if record.Subject != expected.Subject {
		return CandidateRequestRecord{}, execution.ErrSubjectMismatch
	}
	if err = json.Unmarshal(target, &record.Target); err != nil {
		return record, err
	}
	if err = json.Unmarshal(request, &record.Request); err != nil {
		return record, err
	}
	if receipt != nil {
		if err = json.Unmarshal(receipt, &record.Receipt); err != nil {
			return record, err
		}
	}
	return record, record.Verify(ctx, expected.Target, expected.Request)
}

func (s *pgPublicationRequests) findPublication(ctx context.Context, workspaceID, requestID string) (PublicationRequestRecord, bool, error) {
	var record PublicationRequestRecord
	var subjectRaw, commandRaw, receiptRaw []byte
	err := s.pool.QueryRow(ctx, `SELECT actor_subject,request_digest,command,state,receipt FROM weave_team_publication_requests WHERE workspace_id=$1 AND request_id=$2`, workspaceID, requestID).Scan(&subjectRaw, &record.Digest, &commandRaw, &record.State, &receiptRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	if err = json.Unmarshal(subjectRaw, &record.Subject); err != nil {
		return record, false, err
	}
	if err = json.Unmarshal(commandRaw, &record.Command); err != nil {
		return record, false, err
	}
	if len(receiptRaw) > 0 && string(receiptRaw) != "null" {
		if err = json.Unmarshal(receiptRaw, &record.Receipt); err != nil {
			return record, false, err
		}
	}
	if err = record.Verify(ctx, record.Command); err != nil {
		return record, false, err
	}
	return record, true, nil
}

func (s *pgPublicationRequests) findCandidate(ctx context.Context, workspaceID, requestID string) (CandidateRequestRecord, bool, error) {
	var record CandidateRequestRecord
	var subjectRaw, targetRaw, requestRaw, receiptRaw []byte
	err := s.pool.QueryRow(ctx, `SELECT actor_subject,request_digest,target,request,receipt FROM weave_team_candidate_requests WHERE workspace_id=$1 AND request_id=$2`, workspaceID, requestID).Scan(&subjectRaw, &record.Digest, &targetRaw, &requestRaw, &receiptRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	if err = json.Unmarshal(subjectRaw, &record.Subject); err != nil {
		return record, false, err
	}
	if err = json.Unmarshal(targetRaw, &record.Target); err != nil {
		return record, false, err
	}
	if err = json.Unmarshal(requestRaw, &record.Request); err != nil {
		return record, false, err
	}
	if len(receiptRaw) > 0 && string(receiptRaw) != "null" {
		if err = json.Unmarshal(receiptRaw, &record.Receipt); err != nil {
			return record, false, err
		}
	}
	if err = record.Verify(ctx, record.Target, record.Request); err != nil {
		return record, false, err
	}
	return record, true, nil
}

// findCandidateForBuild resolves product provenance from the durable admission
// association. The kernel snapshot deliberately carries no builder round data.
func (s *pgPublicationRequests) findCandidateForBuild(ctx context.Context, workspaceID, buildRunID string, roundNo int, sourceRole, runID, contentHash string) (CandidateRequestRecord, error) {
	if _, err := execution.RequireSubject(ctx, workspaceID); err != nil {
		return CandidateRequestRecord{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT request_id FROM weave_team_candidate_requests WHERE workspace_id=$1 AND target->>'build_run_id'=$2 AND (target->>'round_no')::int=$3 AND target->>'source_role'=$4 AND receipt IS NOT NULL AND ($5='' OR receipt->>'run_id'=$5) AND ($6='' OR request->'candidate'->>'content_hash'=$6) ORDER BY request_id`, workspaceID, buildRunID, roundNo, sourceRole, runID, contentHash)
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return CandidateRequestRecord{}, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return CandidateRequestRecord{}, err
	}
	if len(ids) == 0 {
		return CandidateRequestRecord{}, publication.ErrInvalidReceipt
	}
	var result CandidateRequestRecord
	for _, id := range ids {
		record, found, err := s.findCandidate(ctx, workspaceID, id)
		if err != nil {
			return CandidateRequestRecord{}, err
		}
		if !found || record.Receipt == nil {
			return CandidateRequestRecord{}, publication.ErrInvalidReceipt
		}
		if result.Receipt != nil && (result.Target.ExpectedAssetVersion != record.Target.ExpectedAssetVersion || result.Request.Candidate.ContentHash != record.Request.Candidate.ContentHash) {
			return CandidateRequestRecord{}, publication.ErrRequestConflict
		}
		result = record
	}
	return result, nil
}
