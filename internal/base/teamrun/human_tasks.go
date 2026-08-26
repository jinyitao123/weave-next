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
		r.created_at,r.updated_at,
		COALESCE(c.value->'completed_outputs','{}'::jsonb)
		FROM weave_team_runs r
		LEFT JOIN loom_store c
		  ON c.namespace='teamrun-checkpoint:'||r.workspace_id AND c.key=r.run_id
		WHERE r.workspace_id=$1 AND r.status='parked' AND r.wait_kind='human'
		  AND ($2::timestamptz IS NULL OR (r.updated_at,r.run_id)<($2,$3))
		ORDER BY r.updated_at DESC,r.run_id DESC
		LIMIT $4`, workspaceID, beforeUpdatedAt, beforeRunID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list human tasks: %w", err)
	}
	defer rows.Close()
	items := make([]HumanTaskItem, 0, limit+1)
	for rows.Next() {
		item, scanErr := scanHumanTask(rows)
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
		r.created_at,r.updated_at,
		COALESCE(c.value->'completed_outputs','{}'::jsonb)
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
	var item HumanTaskItem
	var projectID *string
	var status, sourceKind string
	var outputs json.RawMessage
	if err := row.Scan(
		&item.Run.WorkspaceID, &projectID, &item.Run.RunID, &status,
		&item.Run.TeamID, &item.Run.WorkflowID, &item.Run.WorkflowVersion,
		&item.Run.RunSnapshotID, &sourceKind, &item.Run.WaitDetail,
		&item.Run.CreatedAt, &item.Run.UpdatedAt, &outputs,
	); err != nil {
		return HumanTaskItem{}, err
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
		return HumanTaskItem{}, fmt.Errorf("decode stored human task %q: %w", item.Run.RunID, err)
	}
	if err := json.Unmarshal(outputs, &item.CompletedOutputs); err != nil {
		return HumanTaskItem{}, fmt.Errorf("decode human task outputs %q: %w", item.Run.RunID, err)
	}
	if item.CompletedOutputs == nil {
		item.CompletedOutputs = make(map[string]json.RawMessage)
	}
	item.Detail = detail
	return item, nil
}
