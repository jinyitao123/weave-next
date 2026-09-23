package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

type workbenchRunRecord struct {
	RunID           string
	Status          string
	ProjectID       string
	Agent           string
	TeamID          string
	WorkflowID      string
	WorkflowVersion int
	StartedAt       time.Time
	EndedAt         *time.Time
	UpdatedAt       time.Time
}

const workbenchRunRelation = `
	SELECT DISTINCT ON (input.consumed_run_id)
		input.workspace_id, input.user_id, input.project_id AS client_project_id,
		input.consumed_run_id AS run_id,
		CASE
			WHEN team_run.status IS NOT NULL THEN team_run.status
			WHEN task.status IN ('queued','dispatched') THEN 'queued'
			WHEN task.status='running' THEN 'running'
			WHEN task.status='cancel_requested' THEN 'cancel_requested'
			WHEN task.status='completed' THEN 'succeeded'
			WHEN task.status='failed' THEN 'failed'
			WHEN task.status IN ('cancelled','superseded','cut') THEN 'cancelled'
			WHEN task.status='timed_out' THEN 'failed'
			ELSE 'queued'
		END AS status,
		COALESCE(NULLIF(team_run.project_id,''), NULLIF(task.project_id,''), NULLIF(run_snapshot.project_id,''), '') AS project_id,
		COALESCE(NULLIF(workflow.name,''), NULLIF(team.name,''), NULLIF(task.agent,''), '') AS agent,
		COALESCE(team_run.team_id,run_snapshot.team_id,'') AS team_id,
		COALESCE(team_run.workflow_id,run_snapshot.workflow_id,'') AS workflow_id,
		COALESCE(team_run.workflow_version,run_snapshot.workflow_version,0) AS workflow_version,
		COALESCE(team_run.created_at, task.started_at, task.created_at, input.consumed_at) AS started_at,
		COALESCE(team_run.terminal_at, task.completed_at) AS ended_at,
		GREATEST(COALESCE(team_run.updated_at,input.consumed_at), COALESCE(task.updated_at,input.consumed_at)) AS updated_at
	FROM weave_dispatch_input_revisions AS input
	LEFT JOIN weave_task_queue AS task
	  ON task.workspace_id=input.workspace_id AND task.id=input.consumed_task_id
	LEFT JOIN weave_team_runs AS team_run
	  ON team_run.workspace_id=input.workspace_id AND team_run.run_id=input.consumed_run_id
	LEFT JOIN weave_team_run_snapshots AS run_snapshot
	  ON run_snapshot.workspace_id=input.workspace_id AND run_snapshot.run_id=input.consumed_run_id
	LEFT JOIN weave_teams AS team
	  ON team.workspace_id=input.workspace_id AND team.id=COALESCE(team_run.team_id,run_snapshot.team_id)
	LEFT JOIN weave_team_workflows AS workflow
	  ON workflow.workspace_id=input.workspace_id
	 AND workflow.team_id=COALESCE(team_run.team_id,run_snapshot.team_id)
	 AND workflow.id=COALESCE(team_run.workflow_id,run_snapshot.workflow_id)
	WHERE input.workspace_id=$1
	  AND input.user_id=$2
	  AND input.project_id=$3
	  AND input.project_id='workbench-'||input.user_id
	  AND input.consumed_run_id IS NOT NULL
	ORDER BY input.consumed_run_id,input.consumed_at DESC,input.input_revision_id DESC`

func workbenchProjectID(userID string) string {
	return "workbench-" + userID
}

func (s *Server) listWorkbenchRuns(ctx context.Context, workspaceID, userID, projectID string, limit, offset int) ([]RunSummary, int, error) {
	pool := s.GetPool()
	if pool == nil || userID == "" || projectID != workbenchProjectID(userID) {
		return nil, 0, errors.New("workbench run ownership is unavailable")
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (`+workbenchRunRelation+`) AS owned_runs`, workspaceID, userID, projectID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count Workbench runs: %w", err)
	}
	rows, err := pool.Query(ctx, `SELECT run_id,status,project_id,agent,team_id,workflow_id,workflow_version,started_at,ended_at,updated_at
		FROM (`+workbenchRunRelation+`) AS owned_runs
		ORDER BY updated_at DESC,run_id
		LIMIT $4 OFFSET $5`, workspaceID, userID, projectID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list Workbench runs: %w", err)
	}
	defer rows.Close()
	runs := make([]RunSummary, 0, limit)
	for rows.Next() {
		var item workbenchRunRecord
		if err := rows.Scan(&item.RunID, &item.Status, &item.ProjectID, &item.Agent, &item.TeamID, &item.WorkflowID, &item.WorkflowVersion, &item.StartedAt, &item.EndedAt, &item.UpdatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan Workbench run: %w", err)
		}
		runs = append(runs, s.workbenchRunSummary(ctx, workspaceID, item))
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate Workbench runs: %w", err)
	}
	return runs, total, nil
}

func (s *Server) workbenchRunAccess(ctx context.Context, workspaceID, userID, runID string) (workbenchRunRecord, bool, bool, error) {
	pool := s.GetPool()
	if pool == nil {
		return workbenchRunRecord{}, false, false, nil
	}
	var bound bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM weave_dispatch_input_revisions AS input
		WHERE input.workspace_id=$1 AND input.consumed_run_id=$2
		  AND input.project_id='workbench-'||input.user_id
	)`, workspaceID, runID).Scan(&bound); err != nil {
		return workbenchRunRecord{}, false, false, fmt.Errorf("check Workbench run ownership: %w", err)
	}
	if !bound || userID == "" {
		return workbenchRunRecord{}, false, bound, nil
	}
	var item workbenchRunRecord
	err := pool.QueryRow(ctx, `SELECT run_id,status,project_id,agent,team_id,workflow_id,workflow_version,started_at,ended_at,updated_at
		FROM (`+workbenchRunRelation+`) AS owned_runs
		WHERE run_id=$4`, workspaceID, userID, workbenchProjectID(userID), runID).Scan(
		&item.RunID, &item.Status, &item.ProjectID, &item.Agent, &item.TeamID, &item.WorkflowID, &item.WorkflowVersion, &item.StartedAt, &item.EndedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return workbenchRunRecord{}, false, bound, nil
	}
	if err != nil {
		return workbenchRunRecord{}, false, bound, fmt.Errorf("read owned Workbench run: %w", err)
	}
	return item, true, bound, nil
}

func (s *Server) workbenchRunSummary(ctx context.Context, workspaceID string, item workbenchRunRecord) RunSummary {
	summary := RunSummary{
		RunID: item.RunID, Status: item.Status, DurationMs: 0,
		StartedAt: item.StartedAt.UTC().Format(time.RFC3339Nano),
		Agent:     item.Agent, ProjectID: item.ProjectID, Attribution: "project_attributed",
	}
	if item.EndedAt != nil {
		summary.EndedAt = item.EndedAt.UTC().Format(time.RFC3339Nano)
		summary.DurationMs = item.EndedAt.Sub(item.StartedAt).Milliseconds()
	}
	if s.Store != nil {
		if data, err := s.Store.Get(ctx, "audit:"+workspaceID, item.RunID); err == nil {
			var recorded RunSummary
			if json.Unmarshal(data, &recorded) == nil && recorded.RunID == item.RunID {
				summary = recorded
				summary.Status = item.Status
				if item.ProjectID != "" {
					summary.ProjectID = item.ProjectID
				}
				if summary.StartedAt == "" {
					summary.StartedAt = item.StartedAt.UTC().Format(time.RFC3339Nano)
				}
				if summary.EndedAt == "" && item.EndedAt != nil {
					summary.EndedAt = item.EndedAt.UTC().Format(time.RFC3339Nano)
				}
				if summary.DurationMs == 0 && item.EndedAt != nil {
					summary.DurationMs = item.EndedAt.Sub(item.StartedAt).Milliseconds()
				}
				summary.Attribution = "project_attributed"
			}
		}
	}
	return summary
}

func workbenchRunDetail(s *Server, ctx context.Context, workspaceID string, summary RunSummary) map[string]any {
	run := map[string]any{}
	if s.Store != nil {
		if data, err := s.Store.Get(ctx, "audit:"+workspaceID, summary.RunID); err == nil {
			_ = json.Unmarshal(data, &run)
		}
	}
	if len(run) == 0 {
		encoded, _ := json.Marshal(summary)
		_ = json.Unmarshal(encoded, &run)
	}
	run["run_id"], run["status"] = summary.RunID, summary.Status
	run["started_at"], run["attribution"] = summary.StartedAt, summary.Attribution
	run["duration_ms"] = summary.DurationMs
	if summary.ProjectID != "" {
		run["project_id"] = summary.ProjectID
	}
	if summary.Agent != "" {
		run["agent"] = summary.Agent
	}
	if summary.EndedAt != "" {
		run["ended_at"] = summary.EndedAt
	}
	return run
}

func workbenchRunActivitySummary(item workbenchRunRecord) map[string]any {
	observedAt := time.Now().UTC()
	return map[string]any{
		"schema_version": 3, "run_id": item.RunID, "status": item.Status,
		"project_id": item.ProjectID, "team_id": item.TeamID,
		"workflow_id": item.WorkflowID, "workflow_version": item.WorkflowVersion,
		"run_snapshot_id": item.RunID,
		"created_at":      item.StartedAt, "started_at": item.StartedAt,
		"updated_at": item.UpdatedAt, "observed_at": observedAt,
		"members": []runActivityMember{}, "runtimes": []runActivityRuntime{},
		"stages": []runActivityStage{}, "latest_stage": "",
		"completed_stages": 0, "total_stages": 0,
		"human_tasks": []map[string]any{}, "deliverables": []runActivityDeliverableRef{},
		"delivery": map[string]any{
			"verification_status": "pending", "reason": "awaiting_delivery",
			"checks": []map[string]any{}, "check_counts": map[string]int{},
			"available": false, "evidence_completeness": "unavailable",
		},
		"activity_events": []teamrun.ActivityEvent{}, "corrections": []teamrun.Correction{},
		"completeness": map[string]string{
			"run": "complete", "stages": "unavailable", "members": "unavailable",
			"runtimes": "unavailable", "human_tasks": "complete", "deliverables": "unavailable",
			"member_inputs": "unavailable", "member_outputs": "unavailable", "member_tool_activity": "unavailable",
			"activity_events": "unavailable", "corrections": "unavailable", "usage": "unavailable",
		},
	}
}
