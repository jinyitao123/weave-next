package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

// employeeHumanReviewBatch bounds how many human waits one sweep turns into
// events; the rest are picked up by the next sweep.
const employeeHumanReviewBatch = 100

// materializeHumanReviewEvents turns each human wait of a Workbench run into
// one human_review event for the employee who started the work (decision 002).
// The native inbox is the only employee entry; Weave never routes the wait to
// another business handler. One event exists per interaction; a later wait of
// the same run is a new interaction and therefore a new event.
func (worker *employeeRunEventWorker) materializeHumanReviewEvents(ctx context.Context) error {
	rows, err := worker.Pool.Query(ctx, `SELECT run.workspace_id,run.run_id,run.wait_detail,run.team_run_generation,
			run.resume_generation,run.updated_at,input.input_revision_id,input.workbench_session_id,
			COALESCE(identity.subject,''),COALESCE(NULLIF(input.native_organization,''),identity.native_organization,''),
			identity.bindings,
			COALESCE(NULLIF(workflow.name,''),NULLIF(team.name,''),'团队工作')
		FROM weave_team_runs AS run
		JOIN weave_dispatch_input_revisions AS input
		  ON input.workspace_id=run.workspace_id AND input.consumed_run_id=run.run_id
		JOIN LATERAL (SELECT min(subject) AS subject,min(native_organization) AS native_organization,count(*) AS bindings
		  FROM weave_external_identities WHERE workspace_id=input.workspace_id AND user_id=input.user_id
		    AND native_organization<>''
		    AND (input.native_organization='' OR native_organization=input.native_organization)) AS identity ON true
		LEFT JOIN weave_teams AS team
		  ON team.workspace_id=run.workspace_id AND team.id=run.team_id
		LEFT JOIN weave_team_workflows AS workflow
		  ON workflow.workspace_id=run.workspace_id AND workflow.team_id=run.team_id AND workflow.id=run.workflow_id
		WHERE run.status='parked' AND run.wait_kind='human'
		ORDER BY run.updated_at,run.run_id LIMIT $1`, employeeHumanReviewBatch)
	if err != nil {
		return fmt.Errorf("list human waits: %w", err)
	}
	type wait struct {
		run             teamrun.TeamRun
		updatedAt       time.Time
		inputRevisionID string
		sessionID       string
		assignee        string
		organization    string
		teamName        string
		bindings        int
	}
	var waits []wait
	for rows.Next() {
		var item wait
		human := teamrun.WaitHuman
		item.run.Status = teamrun.StatusParked
		item.run.WaitKind = &human
		if err := rows.Scan(&item.run.WorkspaceID, &item.run.RunID, &item.run.WaitDetail, &item.run.Generation,
			&item.run.ResumeGeneration, &item.updatedAt, &item.inputRevisionID, &item.sessionID,
			&item.assignee, &item.organization, &item.bindings, &item.teamName); err != nil {
			rows.Close()
			return err
		}
		waits = append(waits, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range waits {
		if item.bindings != 1 || item.assignee == "" || item.organization == "" {
			return fmt.Errorf("human review run %s has no unique verified native employee organization", item.run.RunID)
		}
		interaction := teamrun.HumanInteractionID(item.run)
		detail, err := teamrun.DecodeHumanWaitDetailV1(item.run.WaitDetail)
		if interaction == "" || err != nil {
			continue
		}
		summary := strings.TrimSpace(detail.Task.Title)
		if instructions := strings.TrimSpace(detail.Task.Instructions); instructions != "" {
			summary = strings.TrimSpace(summary + "：" + instructions)
		}
		if summary == "" {
			summary = "团队工作等待你确认后继续。"
		}
		eventID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("weave-team-run-human-review\x1f"+item.run.WorkspaceID+"\x1f"+interaction)).String()
		payload, err := json.Marshal(map[string]any{
			"version": "1", "eventId": eventID, "kind": "human_review",
			"organizationId": item.organization, "assigneeAccountId": item.assignee,
			"title":      truncateRunes("团队工作等待处理："+item.teamName, 300),
			"summary":    truncateRunes(summary, 4000),
			"occurredAt": item.updatedAt.UTC().Format(time.RFC3339Nano),
			"source": map[string]string{
				"workReference": item.inputRevisionID, "runReference": item.run.RunID,
				"sessionReference": item.sessionID, "idempotencyKey": "weave-team-run-human:" + interaction,
				"interactionReference": interaction,
			},
		})
		if err != nil {
			return err
		}
		if _, err := worker.Pool.Exec(ctx, `INSERT INTO weave_employee_run_event_outbox(event_id,workspace_id,run_id,input_revision_id,payload,event_scope)
			VALUES($1,$2,$3,$4,$5::jsonb,$6)
			ON CONFLICT (workspace_id,run_id,event_scope) DO NOTHING`,
			eventID, item.run.WorkspaceID, item.run.RunID, item.inputRevisionID, string(payload), interaction); err != nil {
			return fmt.Errorf("materialize human review event: %w", err)
		}
	}
	return nil
}
