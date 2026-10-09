package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func prepareFeishuHuman(workspace string, input dispatchInputRevision, raw []byte, generation, resumeGeneration int64, payload, key string) (*completeHumanTaskRequest, string, error) {
	if !json.Valid([]byte(payload)) {
		return nil, "确认内容必须是符合当前问题要求的JSON。", nil
	}
	var object any
	_ = json.Unmarshal([]byte(payload), &object)
	if object == nil {
		return nil, "确认内容不能是null。", nil
	}
	_, err := teamrun.DecodeHumanWaitDetailV1(raw)
	if err != nil {
		return nil, "", err
	}
	human := teamrun.WaitHuman
	run := teamrun.TeamRun{RunID: input.ConsumedRunID, WorkspaceID: workspace, Status: teamrun.StatusParked, WaitKind: &human, WaitDetail: raw, Generation: teamrun.TeamRunGeneration(generation), ResumeGeneration: teamrun.ResumeGeneration(resumeGeneration)}
	return &completeHumanTaskRequest{InteractionID: teamrun.HumanInteractionID(run), InputRevisionID: input.InputRevisionID, WorkbenchSessionID: input.WorkbenchSessionID, Payload: json.RawMessage(payload), IdempotencyKey: key}, "", nil
}

// Each destination has a durable receipt on the same platform event. Holding
// the event row while sending fences concurrent sweepers; failures keep it pending.
func (s *Server) sweepFeishuNotification(ctx context.Context, app *feishuClient) (int, error) {
	if !app.configured() {
		return 0, nil
	}
	tx, err := s.GetPool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var eventID, openID, workspace, user, runID, inputID, team string
	var raw, permissions []byte
	var attempts int
	err = tx.QueryRow(ctx, `SELECT e.event_id::text,link.open_id,e.workspace_id,link.user_id,e.run_id,e.input_revision_id,e.payload,link.permission_sets,e.feishu_attempts,COALESCE(run.team_id,'')
 FROM weave_employee_run_event_outbox e
 JOIN weave_feishu_messages m ON m.app_id=$1 AND m.run_id=e.run_id AND m.input_revision_id=e.input_revision_id AND m.workspace_id=e.workspace_id
 JOIN weave_feishu_links link ON link.app_id=m.app_id AND link.open_id=m.open_id AND link.workspace_id=m.workspace_id AND link.user_id=m.user_id AND link.expires_at>statement_timestamp()
 JOIN weave_users u ON u.id=link.user_id AND u.tenant_id=link.workspace_id AND NOT u.disabled
 JOIN weave_members member ON member.workspace_id=link.workspace_id AND member.user_id=link.user_id
 JOIN weave_dispatch_input_revisions input ON input.workspace_id=e.workspace_id AND input.input_revision_id=e.input_revision_id AND input.user_id=link.user_id AND input.is_current AND input.closed_at IS NULL
 LEFT JOIN weave_team_runs run ON run.workspace_id=e.workspace_id AND run.run_id=e.run_id
 WHERE e.feishu_state='pending' AND e.feishu_next_attempt_at<=statement_timestamp()
   AND ($2 OR NOT EXISTS(SELECT 1 FROM weave_feishu_apps own WHERE own.workspace_id=link.workspace_id))
 ORDER BY e.created_at,e.event_id FOR UPDATE OF e SKIP LOCKED LIMIT 1`, app.appID, app.workspace != "").Scan(&eventID, &openID, &workspace, &user, &runID, &inputID, &raw, &permissions, &attempts, &team)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var payload struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
		Kind    string `json:"kind"`
		Source  struct {
			Interaction string `json:"interactionReference"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return 0, errors.New("feishu notification payload invalid")
	}
	// The team chose which kinds reach Feishu; others are settled, not retried.
	if team != "" {
		access, err := s.feishuTeamAccess(ctx, tx, workspace, team)
		if err != nil {
			return 0, err
		}
		if !access.Notify.allows(payload.Kind) {
			if _, err = tx.Exec(ctx, `UPDATE weave_employee_run_event_outbox SET feishu_state='suppressed',feishu_last_error=NULL WHERE event_id=$1`, eventID); err != nil {
				return 0, err
			}
			return 1, tx.Commit(ctx)
		}
	}
	text := payload.Title
	if payload.Kind == "human_review" {
		var wait []byte
		var kind string
		var generation, resumeGeneration int64
		err = s.GetPool().QueryRow(ctx, `SELECT COALESCE(wait_kind,''),wait_detail,team_run_generation,resume_generation FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2 AND status='parked'`, workspace, runID).Scan(&kind, &wait, &generation, &resumeGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			text = "原确认已处理，请发送“查看”读取最新状态。"
		} else if err != nil {
			return 0, err
		} else {
			human := teamrun.WaitHuman
			run := teamrun.TeamRun{RunID: runID, WorkspaceID: workspace, Status: teamrun.StatusParked, WaitKind: &human, WaitDetail: wait, Generation: teamrun.TeamRunGeneration(generation), ResumeGeneration: teamrun.ResumeGeneration(resumeGeneration)}
			if kind != "human" || teamrun.HumanInteractionID(run) != payload.Source.Interaction {
				text = "原确认已失效，请发送“查看”读取最新状态。"
			} else {
				detail, err := teamrun.DecodeHumanWaitDetailV1(wait)
				if err != nil {
					return 0, err
				}
				text += "\n" + truncateRunes(payload.Summary, 1500) + "\n确认内容格式：" + truncateRunes(string(detail.ResumeSchema), 1500) + "\n发送：确认 JSON"
			}
		}
	} else {
		text += "\n发送“查看”读取本轮成果；发送“继续 补充要求”修订原工作。"
	}
	id, sendErr := app.send(ctx, openID, feishuMessageKey(app.appID, eventID), text)
	if sendErr != nil {
		_, err = tx.Exec(ctx, `UPDATE weave_employee_run_event_outbox SET feishu_attempts=feishu_attempts+1,feishu_last_error=$2,feishu_next_attempt_at=statement_timestamp()+($3::text||' seconds')::interval WHERE event_id=$1`, eventID, sendErr.Error(), fmt.Sprint(1<<min(attempts+1, 8)))
	} else {
		_, err = tx.Exec(ctx, `UPDATE weave_employee_run_event_outbox SET feishu_state='delivered',feishu_attempts=feishu_attempts+1,feishu_message_id=$2,feishu_last_error=NULL WHERE event_id=$1`, eventID, id)
	}
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, err
	}
	return 1, sendErr
}

func (worker *employeeRunEventWorker) sweepFeishu(ctx context.Context) (int, error) {
	if worker.FeishuServer == nil {
		return 0, nil
	}
	apps, appsErr := worker.FeishuServer.feishuClients(ctx)
	total, failures := 0, []error{appsErr}
	for _, app := range apps {
		commandCount, commandErr := worker.FeishuServer.sweepFeishuCommand(ctx, app)
		notificationCount, notificationErr := worker.FeishuServer.sweepFeishuNotification(ctx, app)
		total += commandCount + notificationCount
		failures = append(failures, commandErr, notificationErr)
	}
	return total, errors.Join(failures...)
}
