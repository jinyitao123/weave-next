package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type HumanTaskItem struct {
	Run              TeamRun
	Detail           HumanWaitDetailV1
	CompletedOutputs map[string]json.RawMessage
}

type HumanTaskReader struct {
	Pool *pgxpool.Pool
	// InputUnboundOnly limits the developer inbox to standalone test runs.
	// Input-bound work reaches its owner through the client's native inbox.
	InputUnboundOnly bool
}

func (r *HumanTaskReader) Count(ctx context.Context, workspaceID string) (int, error) {
	if r == nil || r.Pool == nil || workspaceID == "" {
		return 0, errors.New("human task reader and workspace are required")
	}
	var count int
	if err := r.Pool.QueryRow(ctx, `SELECT count(*) FROM weave_team_runs
		WHERE workspace_id=$1 AND status='parked' AND wait_kind='human'
		  AND (NOT $2::boolean OR NOT EXISTS (SELECT 1 FROM weave_dispatch_input_revisions i
		    WHERE i.workspace_id=weave_team_runs.workspace_id AND i.consumed_run_id=weave_team_runs.run_id))`,
		workspaceID, r.InputUnboundOnly).Scan(&count); err != nil {
		return 0, fmt.Errorf("count human tasks: %w", err)
	}
	return count, nil
}

func (r *HumanTaskReader) List(
	ctx context.Context,
	workspaceID string,
	beforeUpdatedAt *time.Time,
	beforeRunID string,
	limit int,
) ([]HumanTaskItem, bool, error) {
	if r == nil || r.Pool == nil || workspaceID == "" {
		return nil, false, errors.New("human task reader and workspace are required")
	}
	if limit < 1 || limit > 100 {
		return nil, false, errors.New("human task limit must be between 1 and 100")
	}
	rows, err := r.Pool.Query(ctx, `SELECT
		r.workspace_id,r.project_id,r.run_id,r.status,r.team_id,r.workflow_id,
		r.workflow_version,r.run_snapshot_id,r.source_kind,r.wait_detail,
		r.created_at,r.updated_at,r.team_run_generation,r.resume_generation
		FROM weave_team_runs r
		WHERE r.workspace_id=$1 AND r.status='parked' AND r.wait_kind='human'
		  AND ($2::timestamptz IS NULL OR (r.updated_at,r.run_id)<($2,$3))
		  AND (NOT $5::boolean OR NOT EXISTS (SELECT 1 FROM weave_dispatch_input_revisions i
		    WHERE i.workspace_id=r.workspace_id AND i.consumed_run_id=r.run_id))
		ORDER BY r.updated_at DESC,r.run_id DESC
		LIMIT $4`, workspaceID, beforeUpdatedAt, beforeRunID, limit+1, r.InputUnboundOnly)
	if err != nil {
		return nil, false, fmt.Errorf("list human tasks: %w", err)
	}
	defer rows.Close()
	items := make([]HumanTaskItem, 0, limit+1)
	for rows.Next() {
		item, scanErr := scanHumanTaskSummary(rows)
		if scanErr != nil {
			return nil, false, scanErr
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list human task rows: %w", err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
}

func (r *HumanTaskReader) Get(
	ctx context.Context,
	workspaceID string,
	runID string,
) (HumanTaskItem, error) {
	if r == nil || r.Pool == nil || workspaceID == "" || runID == "" {
		return HumanTaskItem{}, errors.New("human task reader, workspace, and run are required")
	}
	item, err := scanHumanTask(r.Pool.QueryRow(ctx, `SELECT
		r.workspace_id,r.project_id,r.run_id,r.status,r.team_id,r.workflow_id,
		r.workflow_version,r.run_snapshot_id,r.source_kind,r.wait_detail,
		r.created_at,r.updated_at,r.team_run_generation,r.resume_generation,c.value
		FROM weave_team_runs r
		LEFT JOIN loom_store c
		  ON c.namespace='teamrun-checkpoint:'||r.workspace_id AND c.key=r.run_id
		WHERE r.workspace_id=$1 AND r.run_id=$2 AND r.status='parked' AND r.wait_kind='human'`,
		workspaceID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return HumanTaskItem{}, fmt.Errorf("%w: human task not found", ErrTeamRunIdentityMismatch)
	}
	return item, err
}

func scanHumanTask(row rowScanner) (HumanTaskItem, error) {
	item, checkpointRaw, err := scanHumanTaskRow(row, true)
	if err != nil {
		return HumanTaskItem{}, err
	}
	checkpoint, err := DecodeWorkflowCheckpointV1(checkpointRaw)
	if err != nil {
		return HumanTaskItem{}, fmt.Errorf("decode human task checkpoint %q: %w", item.Run.RunID, err)
	}
	item.CompletedOutputs = checkpoint.CompletedOutputs
	if item.CompletedOutputs == nil {
		item.CompletedOutputs = make(map[string]json.RawMessage)
	}
	return item, nil
}

func scanHumanTaskSummary(row rowScanner) (HumanTaskItem, error) {
	item, _, err := scanHumanTaskRow(row, false)
	return item, err
}

func scanHumanTaskRow(row rowScanner, withCheckpoint bool) (HumanTaskItem, []byte, error) {
	var item HumanTaskItem
	var projectID *string
	var status, sourceKind string
	var checkpointRaw []byte
	targets := []any{
		&item.Run.WorkspaceID, &projectID, &item.Run.RunID, &status,
		&item.Run.TeamID, &item.Run.WorkflowID, &item.Run.WorkflowVersion,
		&item.Run.RunSnapshotID, &sourceKind, &item.Run.WaitDetail,
		&item.Run.CreatedAt, &item.Run.UpdatedAt, &item.Run.Generation, &item.Run.ResumeGeneration,
	}
	if withCheckpoint {
		targets = append(targets, &checkpointRaw)
	}
	if err := row.Scan(targets...); err != nil {
		return HumanTaskItem{}, nil, err
	}
	if projectID != nil {
		item.Run.ProjectID = *projectID
	}
	item.Run.Status = Status(status)
	item.Run.SourceKind = SourceKind(sourceKind)
	waitKind := WaitHuman
	item.Run.WaitKind = &waitKind
	detail, err := DecodeHumanWaitDetailV1(item.Run.WaitDetail)
	if err != nil {
		return HumanTaskItem{}, nil, fmt.Errorf("decode stored human task %q: %w", item.Run.RunID, err)
	}
	item.Detail = detail
	return item, checkpointRaw, nil
}
