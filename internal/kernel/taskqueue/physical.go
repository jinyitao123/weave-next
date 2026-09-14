package taskqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jinyitao123/weave/internal/base/execution"
	"math"
)

// RecordClaimUsage records one physical attempt, including an expired attempt
// whose owner is retained. Replay is idempotent and a later owner is fenced.
// A missing receipt stays explicitly unreported; it is never counted as zero.
func (s *Store) RecordClaimUsage(ctx context.Context, id, workerID string, epoch int64, usage *execution.TerminalUsage) error {
	return s.recordClaimUsage(ctx, s.pool, id, workerID, epoch, usage)
}

type claimUsageQuerier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Store) recordClaimUsage(ctx context.Context, q claimUsageQuerier, id, workerID string, epoch int64, usage *execution.TerminalUsage) error {
	if usage == nil {
		return nil
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ToolCalls < 0 || usage.CostUSD < 0 || math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) {
		return errors.New("invalid physical usage")
	}
	tag, err := q.Exec(ctx, `UPDATE weave_task_queue SET
 physical_usage=jsonb_build_object('input_tokens',COALESCE((physical_usage->>'input_tokens')::bigint,0)+$4::bigint,
 'output_tokens',COALESCE((physical_usage->>'output_tokens')::bigint,0)+$5::bigint,
 'cost_usd',COALESCE((physical_usage->>'cost_usd')::numeric,0)+$6::numeric,
 'tool_calls',COALESCE((physical_usage->>'tool_calls')::bigint,0)+$7::bigint),
 usage_epoch=$3,unreported_attempts=unreported_attempts-1
 WHERE id=$1 AND (worker_id=$2 OR (worker_id IS NULL AND stopped_worker_id=$2 AND stopped_epoch=$3)) AND claim_epoch=$3 AND usage_epoch<$3 AND unreported_attempts>0`, id, workerID, epoch, usage.InputTokens, usage.OutputTokens, usage.CostUSD, usage.ToolCalls)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var replay bool
	err = q.QueryRow(ctx, `SELECT claim_epoch=$3 AND usage_epoch=$3 AND (worker_id=$2 OR stopped_worker_id=$2) FROM weave_task_queue WHERE id=$1`, id, workerID, epoch).Scan(&replay)
	if err != nil {
		return err
	}
	if !replay {
		return errors.New("physical usage owner or epoch mismatch")
	}
	return nil
}

// Reconciliation is a business-verified disposition after a physical stop.
// It cannot assert a stop, reset accumulated usage, or extend admission time.
type Reconciliation struct {
	Status string
	Result json.RawMessage
	RunID  string
	Error  string
}

func (s *Store) ReconcileTaskTx(ctx context.Context, tx pgx.Tx, workspaceID, id string, expectedEpoch int64, result Reconciliation) (*Task, error) {
	if tx == nil || expectedEpoch < 1 {
		return nil, errors.New("transaction and physical epoch are required")
	}
	switch result.Status {
	case StatusQueued, StatusCompleted, StatusFailed, StatusCancelled:
	default:
		return nil, errors.New("invalid reconciliation disposition")
	}
	now := s.clock.Now()
	task, err := scanTask(tx.QueryRow(ctx, `UPDATE weave_task_queue SET status=$4,result=$5,run_id=NULLIF($6,''),error=NULLIF($7,''),
 available_at=$8,updated_at=$8,completed_at=CASE WHEN $4='queued' THEN NULL ELSE $8::timestamptz END,
 started_at=CASE WHEN $4='queued' THEN NULL ELSE started_at END
 WHERE workspace_id=$1 AND id=$2 AND claim_epoch=$3 AND stopped_epoch=$3 AND stopped_worker_id IS NOT NULL
 AND worker_id IS NULL AND status IN ('failed','completed','cancelled')
	 AND (status<>'cancelled' OR $4='cancelled')
	 AND ($4<>'queued' OR deadline_at IS NULL OR deadline_at>$8)
	 AND ($4<>'queued' OR parent_task_id IS NULL OR EXISTS (
	   SELECT 1 FROM weave_task_queue parent WHERE parent.workspace_id=weave_task_queue.workspace_id
	   AND parent.id=weave_task_queue.parent_task_id AND parent.actor_subject=weave_task_queue.actor_subject
	   AND parent.status IN ('queued','dispatched','running')))
 AND ($9::jsonb IS NULL OR actor_subject=$9::jsonb)
 RETURNING `+taskColumns, workspaceID, id, expectedEpoch, result.Status, result.Result, result.RunID, result.Error, now, subjectFilter(ctx)))
	if err != nil {
		return nil, fmt.Errorf("task requires a matching physical stop receipt before reconciliation: %w", err)
	}
	return task, nil
}
