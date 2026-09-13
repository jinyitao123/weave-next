package teamconstruction

import (
	"context"
	"errors"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/build/teameval"
)

type candidateEvidenceReader struct {
	requests  *pgPublicationRequests
	snapshots *snapshot.Store
}

func (r candidateEvidenceReader) ListByTeam(ctx context.Context, workspaceID, teamID string) ([]teameval.CandidateSnapshotEvidence, error) {
	if r.requests == nil || r.snapshots == nil {
		return nil, errors.New("candidate evidence reader unavailable")
	}
	if _, err := execution.RequireSubject(ctx, workspaceID); err != nil {
		return nil, err
	}
	rows, err := r.requests.pool.Query(ctx, `SELECT request_id FROM weave_team_candidate_requests WHERE workspace_id=$1 AND receipt IS NOT NULL ORDER BY created_at,request_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]teameval.CandidateSnapshotEvidence, 0, len(ids))
	for _, id := range ids {
		record, found, err := r.requests.findCandidate(ctx, workspaceID, id)
		if errors.Is(err, execution.ErrSubjectMismatch) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !found || record.Receipt == nil {
			return nil, errors.New("candidate association lost its receipt")
		}
		payload, err := frozen.DecodeArtifactEnvelopeV1(record.Request.Candidate)
		if err != nil {
			return nil, err
		}
		if payload.Team.TeamID != teamID {
			continue
		}
		runSnapshot, err := r.snapshots.GetByRunID(ctx, workspaceID, record.Receipt.RunSnapshotID)
		if err != nil {
			return nil, err
		}
		if runSnapshot.Subject != record.Subject || runSnapshot.TeamID != teamID || runSnapshot.RunID != record.Receipt.RunID || runSnapshot.SourceRef != record.Request.SourceRef || runSnapshot.CandidateContentHash != record.Request.Candidate.ContentHash || runSnapshot.WorkflowID != record.Request.Candidate.WorkflowID || runSnapshot.WorkflowVersion != record.Request.Candidate.WorkflowVersion {
			return nil, errors.New("candidate evidence identity differs from its immutable admission")
		}
		result = append(result, teameval.CandidateSnapshotEvidence{RunID: runSnapshot.RunID, WorkflowID: runSnapshot.WorkflowID, WorkflowVersion: runSnapshot.WorkflowVersion, BuildRunID: record.Target.BuildRunID, BuildRoundNo: record.Target.RoundNo, CreatedAt: runSnapshot.CreatedAt})
	}
	return result, nil
}
