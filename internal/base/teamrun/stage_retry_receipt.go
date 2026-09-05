package teamrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func stageRetryReceiptID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "stage-retry-request:" + hex.EncodeToString(digest[:])
}

type stageRetryReceipt struct {
	SchemaVersion           int               `json:"schema_version"`
	FailureGeneration       TeamRunGeneration `json:"failure_generation"`
	FailureResumeGeneration ResumeGeneration  `json:"failure_resume_generation"`
	FailureTaskID           string            `json:"failure_task_id,omitempty"`
	FailureTaskUpdatedAt    *time.Time        `json:"failure_task_updated_at,omitempty"`
	Result                  StageRetryResult  `json:"result"`
}

func replayStageRetryReceiptTx(ctx context.Context, tx pgx.Tx, request StageRetryRequest) (StageRetryResult, bool, error) {
	if request.IdempotencyKey == "" {
		return StageRetryResult{}, false, nil
	}
	var nodeID string
	var raw json.RawMessage
	err := tx.QueryRow(ctx, `SELECT COALESCE(node_id,''),detail FROM weave_team_run_activity_events
 WHERE workspace_id=$1 AND run_id=$2 AND event_id=$3 AND kind='stage_retry_requested'`, request.WorkspaceID, request.RunID, stageRetryReceiptID(request.IdempotencyKey)).Scan(&nodeID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return StageRetryResult{}, false, nil
	}
	if err != nil {
		return StageRetryResult{}, false, err
	}
	var receipt stageRetryReceipt
	if nodeID != request.NodeID || json.Unmarshal(raw, &receipt) != nil || receipt.SchemaVersion != 1 || receipt.Result.RunID != request.RunID || receipt.Result.NodeID != request.NodeID {
		return StageRetryResult{}, false, ErrTeamRunStateConflict
	}
	receipt.Result.Replayed = true
	return receipt.Result, true, nil
}

func recordStageRetryReceiptTx(ctx context.Context, tx pgx.Tx, run TeamRun, request StageRetryRequest, result StageRetryResult) error {
	if request.IdempotencyKey == "" {
		return nil
	} // compatibility for older callers
	receipt := stageRetryReceipt{SchemaVersion: 1, FailureGeneration: run.Generation, FailureResumeGeneration: run.ResumeGeneration,
		FailureTaskID: result.failureTaskID, Result: result}
	if !result.failureTaskUpdatedAt.IsZero() {
		receipt.FailureTaskUpdatedAt = &result.failureTaskUpdatedAt
	}
	detail, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return (&PGActivityStore{}).RecordTx(ctx, tx, ActivityEvent{
		WorkspaceID: request.WorkspaceID, RunID: request.RunID, NodeID: request.NodeID,
		EventID: stageRetryReceiptID(request.IdempotencyKey), Kind: "stage_retry_requested", Detail: detail, OccurredAt: time.Now().UTC(),
	})
}
