package workflowcatalog

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// LockPublicationIntentTx orders product intent creation and pure-draft
// deletion. Callers that lock catalog rows acquire those locks first. The
// immutable intent commits before any kernel call, so deletion can decide from
// product history without calling another owner under its transaction.
func LockPublicationIntentTx(ctx context.Context, tx pgx.Tx, workspaceID, workflowID string) error {
	key, _ := json.Marshal([]string{"workflow-publication-intent", workspaceID, workflowID})
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || $1,0))`, string(key))
	return err
}
