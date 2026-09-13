package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ArtifactStore owns reads of immutable publication facts and the platform's
// admission ledger. Mutable workflow catalogs and draft writes live outside it.
// PublicationService is the sole writer of frozen candidates and revisions.
type ArtifactStore struct {
	pool  *pgxpool.Pool
	clock Clock
}

func NewArtifactStore(pool *pgxpool.Pool, clock Clock) *ArtifactStore {
	if clock == nil {
		clock = RealClock{}
	}
	return &ArtifactStore{pool: pool, clock: clock}
}

var _ PublicationReader = (*ArtifactStore)(nil)

type rowScanner interface {
	Scan(...any) error
}

func (s *ArtifactStore) ReadAdmission(ctx context.Context, workspaceID, workflowID string, version int) (*AdmissionState, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin publication admission read: %w", err)
	}
	defer tx.Rollback(ctx)
	var result AdmissionState
	err = tx.QueryRow(ctx, `SELECT status.blocked,
 CASE WHEN status.blocked AND latest.new_blocked THEN latest.reason ELSE NULL END, latest.created_at
 FROM weave_workflow_version_admission_statuses AS status
 LEFT JOIN LATERAL (
   SELECT reason, new_blocked, created_at FROM weave_workflow_version_admission_audits
   WHERE workspace_id=status.workspace_id AND workflow_id=status.workflow_id
     AND workflow_version=status.workflow_version
   ORDER BY created_at DESC, audit_id COLLATE "C" DESC LIMIT 1
 ) AS latest ON true
 WHERE status.workspace_id=$1 AND status.workflow_id=$2 AND status.workflow_version=$3`,
		workspaceID, workflowID, version).Scan(&result.Blocked, &result.LatestBlockReason, &result.LatestAuditAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: workflow %q version %d admission status", ErrNotFound, workflowID, version)
	}
	if err != nil {
		return nil, fmt.Errorf("read publication admission: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("finish publication admission read: %w", err)
	}
	return &result, nil
}

// HasHistory prevents a product catalog from treating a previously frozen
// workflow as a disposable draft. It neither edits nor locks the catalog.
func (s *ArtifactStore) HasHistory(ctx context.Context, workspaceID, workflowID string) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM weave_published_artifact_contents WHERE workspace_id=$1 AND workflow_id=$2)
 OR EXISTS(SELECT 1 FROM weave_team_workflow_candidates WHERE workspace_id=$1 AND workflow_id=$2)`,
		workspaceID, workflowID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read publication history: %w", err)
	}
	return exists, nil
}
