package workflowcatalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/revocation"
)

// VersionAdmissionView is the current admission read model for one published
// workflow version: the blocked kill switch, the latest audit facts behind it,
// and whether the version's frozen authorization has since been tightened by
// the live roster (the grandfathering signal).
type VersionAdmissionView struct {
	Blocked           bool
	Tightened         bool
	LatestBlockReason *string
	LatestAuditAt     *time.Time
}

// GetVersionAdmissionView reads one version's admission status row, latest
// audit, and roster-tightening facts. Frozen execution facts are read through
// their owner before opening a product-only repeatable-read transaction.
func (s *Store) GetVersionAdmissionView(
	ctx context.Context,
	workspaceID, workflowID string,
	version int,
) (*VersionAdmissionView, error) {
	if s.artifacts == nil {
		return nil, errors.New("frozen publication reader unavailable")
	}
	admission, err := s.artifacts.ReadAdmission(ctx, workspaceID, workflowID, version)
	if err != nil {
		return nil, err
	}
	view := &VersionAdmissionView{Blocked: admission.Blocked, LatestBlockReason: admission.LatestBlockReason, LatestAuditAt: admission.LatestAuditAt}

	artifact, err := s.artifacts.GetArtifact(ctx, workspaceID, workflowID, version)
	if err != nil {
		return nil, fmt.Errorf("read admission view artifact: %w", err)
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
		WorkspaceID:               artifact.WorkspaceID,
		WorkflowID:                artifact.WorkflowID,
		WorkflowVersion:           artifact.WorkflowVersion,
		ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
		CanonicalizationVersion:   artifact.CanonicalizationVersion,
		HashAlgorithm:             artifact.HashAlgorithm,
		ContentHash:               artifact.ContentHash,
		Payload:                   artifact.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("decode admission view artifact: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("begin workflow version admission view read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var teamID string
	if err := tx.QueryRow(ctx, `
		SELECT team_id
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, workflowID).Scan(&teamID); err != nil {
		return nil, fmt.Errorf("read admission view workflow team: %w", err)
	}

	if payload.Team.WorkspaceID != workspaceID || payload.Team.TeamID != teamID {
		return nil, errors.New("admission view artifact team does not match workflow")
	}
	references, err := revocation.ExtractGraphReferences(payload.GraphDefinition)
	if err != nil {
		return nil, fmt.Errorf("decode admission view references: %w", err)
	}

	workerIDs := make([]string, 0, len(references))
	for _, reference := range references {
		if len(workerIDs) == 0 || workerIDs[len(workerIDs)-1] != reference.WorkerAgentID {
			workerIDs = append(workerIDs, reference.WorkerAgentID)
		}
	}
	allowed := make(map[string]map[string]struct{}, len(workerIDs))
	if len(workerIDs) != 0 {
		rows, err := tx.Query(ctx, `
			SELECT worker_agent_id, allowed_kinds
			FROM weave_team_workers
			WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=ANY($3)
		`, workspaceID, teamID, workerIDs)
		if err != nil {
			return nil, fmt.Errorf("read admission view roster kinds: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				workerID string
				kinds    []string
			)
			if err := rows.Scan(&workerID, &kinds); err != nil {
				return nil, fmt.Errorf("scan admission view roster kinds: %w", err)
			}
			current := make(map[string]struct{}, len(kinds))
			for _, kind := range kinds {
				current[kind] = struct{}{}
			}
			allowed[workerID] = current
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate admission view roster kinds: %w", err)
		}
	}
	for _, reference := range references {
		if _, ok := allowed[reference.WorkerAgentID][reference.Kind]; !ok {
			view.Tightened = true
			break
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit workflow version admission view read: %w", err)
	}
	return view, nil
}
