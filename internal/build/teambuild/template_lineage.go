package teambuild

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// GetTemplateLineageRevision finds the immutable template blueprint that
// materialized one unevaluated team. Evaluation planning must use this source
// document because the optimize baseline intentionally omits user-facing
// member design fields and a declarative source spec.
func (s *Store) GetTemplateLineageRevision(
	ctx context.Context,
	workspaceID, teamID string,
) (BlueprintRevision, error) {
	var buildRunID string
	err := s.pool.QueryRow(ctx, `
		SELECT build_run_id
		FROM weave_team_build_runs
		WHERE workspace_id=$1
		  AND execution_strategy='template_instantiate'
		  AND status='passed'
		  AND final_ref_json->>'team_id'=$2
		ORDER BY decided_at DESC, build_run_id DESC
		LIMIT 1
	`, workspaceID, teamID).Scan(&buildRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BlueprintRevision{}, fmt.Errorf("get template lineage revision: %w", ErrBuildRunNotFound)
	}
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("get template lineage revision: %w", err)
	}
	revision, err := s.GetLatestBlueprintRevision(ctx, workspaceID, buildRunID)
	if err != nil {
		return BlueprintRevision{}, fmt.Errorf("get template lineage revision: %w", err)
	}
	return revision, nil
}
