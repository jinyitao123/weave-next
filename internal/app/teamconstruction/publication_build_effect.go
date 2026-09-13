package teamconstruction

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/build/teambuild"
)

// MarkBuildPublicationTx owns the product team certification alongside its
// product build result. Builder persistence never writes the product team table.
func MarkBuildPublicationTx(ctx context.Context, tx pgx.Tx, builds *teambuild.Store, workspaceID, buildRunID, actor string, finalRef teambuild.FinalRef, verifiedBaselineHash string) (teambuild.TeamBuildRun, error) {
	locked, err := builds.LockBuildRunTx(ctx, tx, workspaceID, buildRunID)
	if err != nil {
		return teambuild.TeamBuildRun{}, err
	}
	if locked.EvaluationOnly {
		if locked.Baseline == nil || verifiedBaselineHash == "" || verifiedBaselineHash != locked.Baseline.ContentHash || finalRef.TeamID != locked.EvaluationTeamID {
			return teambuild.TeamBuildRun{}, teambuild.ErrEvaluationPublishCAS
		}
		tag, err := tx.Exec(ctx, `UPDATE weave_teams SET evaluation='evaluated',evaluation_build_run_id=$3,evaluation_contract_hash=$4,evaluated_at=$5,updated_at=$5 WHERE workspace_id=$1 AND id=$2 AND evaluation='unevaluated'`, workspaceID, locked.EvaluationTeamID, buildRunID, locked.ContractHash, time.Now().UTC())
		if err != nil {
			return teambuild.TeamBuildRun{}, err
		}
		if tag.RowsAffected() != 1 {
			return teambuild.TeamBuildRun{}, fmt.Errorf("%w: team state changed", teambuild.ErrEvaluationPublishCAS)
		}
	}
	return builds.MarkPublishedTx(ctx, tx, workspaceID, buildRunID, actor, finalRef, verifiedBaselineHash)
}
