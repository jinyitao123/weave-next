package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/labstack/echo/v4"
)

type feishuActor struct {
	WorkspaceID, UserID, OpenID string
	Permissions                 []string
	App                         *feishuClient
}

// invokeFeishu reuses the employee boundary after a verified, unexpired binding.
// Provider text never chooses an employee, role, service credential or business scope.
func (s *Server) invokeFeishu(ctx context.Context, actor feishuActor, handler echo.HandlerFunc, body any, names, values []string) ([]byte, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(execution.WithSubject(ctx, execution.Subject{WorkspaceID: actor.WorkspaceID, UserID: actor.UserID}), http.MethodPost, "/", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	response := &feishuResponseWriter{headers: make(http.Header), code: http.StatusOK}
	c := s.Echo.NewContext(req, response)
	c.Set("tenant", actor.WorkspaceID)
	c.Set("user_id", actor.UserID)
	c.Set("roles", []string{"member"})
	c.Set(identitySourceContextKey, "forge")
	c.Set(forgePermissionSetsContextKey, actor.Permissions)
	c.SetParamNames(names...)
	c.SetParamValues(values...)
	if err := s.requireCurrentWorkspaceMember(c); err != nil {
		return nil, err
	}
	if err := handler(c); err != nil {
		if httpError, ok := err.(*echo.HTTPError); ok && httpError.Code < 500 {
			return nil, &feishuRejected{code: httpError.Code}
		}
		return nil, err
	}
	if response.code >= 500 {
		return nil, errors.New("feishu work service unavailable")
	}
	if response.code >= 400 {
		return nil, &feishuRejected{code: response.code}
	}
	return response.body.Bytes(), nil
}

type feishuRejected struct{ code int }

func (e *feishuRejected) Error() string { return fmt.Sprintf("work request rejected (%d)", e.code) }

// The callback journal holds only transport facts. This uses the existing
// employee-event sweep; there is no second execution queue or task loop.
func (s *Server) sweepFeishuCommand(ctx context.Context, app *feishuClient) (processed int, outcome error) {
	if !app.configured() {
		return 0, nil
	}
	tx, err := s.GetPool().Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var messageID, openID string
	defer func() {
		if outcome != nil && messageID != "" {
			_ = tx.Rollback(ctx)
			_, _ = s.GetPool().Exec(ctx, `UPDATE weave_feishu_messages SET attempts=attempts+1,next_attempt_at=statement_timestamp()+interval '30 seconds' WHERE app_id=$1 AND message_id=$2`, app.appID, messageID)
		}
	}()
	var workspace, user, response, reply *string
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT m.message_id,m.open_id,m.workspace_id,m.user_id,m.command,m.response,m.reply_message_id FROM weave_feishu_messages AS m
 WHERE m.app_id=$1 AND m.reply_message_id IS NULL AND m.next_attempt_at<=statement_timestamp()
 AND NOT EXISTS(SELECT 1 FROM weave_feishu_messages older WHERE older.app_id=m.app_id AND older.open_id=m.open_id AND older.reply_message_id IS NULL
 AND (older.created_at,older.message_id)<(m.created_at,m.message_id))
 ORDER BY m.created_at,m.message_id FOR UPDATE OF m SKIP LOCKED LIMIT 1`, app.appID).Scan(&messageID, &openID, &workspace, &user, &raw, &response, &reply)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var command feishuCommand
	if json.Unmarshal(raw, &command) != nil {
		return 0, errors.New("feishu command invalid")
	}
	if response != nil {
		// Work replies can contain a cached result. Authorization at command
		// execution does not authorize delivery after expiry, unlink or rebinding.
		// Pairing and unlink acknowledgements contain only fixed, public text.
		if command.BindingCodeHash == "" && strings.TrimSpace(command.Text) != "解绑" {
			var allowed bool
			if workspace != nil && user != nil {
				err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM weave_feishu_links link
 JOIN weave_users u ON u.id=link.user_id AND u.tenant_id=link.workspace_id AND NOT u.disabled
 JOIN weave_members member ON member.workspace_id=link.workspace_id AND member.user_id=link.user_id
 WHERE link.app_id=$1 AND link.open_id=$2 AND link.workspace_id=$3 AND link.user_id=$4
 AND link.expires_at>statement_timestamp())`, app.appID, openID, *workspace, *user).Scan(&allowed)
				if err != nil {
					return 0, err
				}
			}
			if !allowed {
				*response = "绑定已失效或员工当前无权查看，请在桌面重新登录并绑定。"
				// Persist the replacement even if sending fails, so a later binding
				// cannot revive the old account's cached result on retry.
				if _, err = tx.Exec(ctx, `UPDATE weave_feishu_messages SET response=$3 WHERE app_id=$1 AND message_id=$2`, app.appID, messageID, *response); err != nil {
					return 0, err
				}
			}
		}
		id, err := app.send(ctx, openID, feishuMessageKey(app.appID, messageID), *response)
		if err != nil {
			if _, delayErr := tx.Exec(ctx, `UPDATE weave_feishu_messages SET attempts=attempts+1,next_attempt_at=statement_timestamp()+interval '30 seconds' WHERE app_id=$1 AND message_id=$2`, app.appID, messageID); delayErr != nil {
				return 0, delayErr
			}
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return 0, commitErr
			}
			return 0, err
		}
		_, err = tx.Exec(ctx, `UPDATE weave_feishu_messages SET reply_message_id=$3 WHERE app_id=$1 AND message_id=$2`, app.appID, messageID, id)
		if err != nil {
			return 0, err
		}
		return 1, tx.Commit(ctx)
	}
	text := strings.TrimSpace(command.Text)
	result := ""
	inputID, runID := "", ""
	if command.BindingCodeHash != "" {
		tag, err := tx.Exec(ctx, `UPDATE weave_feishu_links SET open_id=$3,chat_id=$4,code_hash=NULL,code_expires_at=NULL
   WHERE app_id=$1 AND code_hash=$2 AND code_expires_at>statement_timestamp() AND expires_at>statement_timestamp()
   AND NOT EXISTS(SELECT 1 FROM weave_feishu_links linked WHERE linked.app_id=$1 AND linked.open_id=$3 AND (linked.workspace_id,linked.user_id)<>(weave_feishu_links.workspace_id,weave_feishu_links.user_id))`, app.appID, command.BindingCodeHash, openID, command.ChatID)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 1 {
			result = "已绑定。发送“团队”查看可用团队；发送“开始 团队名称 任务内容”提交工作。"
		} else {
			result = "绑定码无效、已使用或过期。请重新取得绑定码；切换员工前先解绑。"
		}
	} else {
		actor := feishuActor{OpenID: openID, App: app}
		var permissionJSON []byte
		if workspace == nil || user == nil {
			result = "请先在桌面登录，并绑定本人飞书账号。"
		} else {
			actor.WorkspaceID, actor.UserID = *workspace, *user
			err = tx.QueryRow(ctx, `SELECT link.permission_sets FROM weave_feishu_links link JOIN weave_users u ON u.id=link.user_id AND u.tenant_id=link.workspace_id
    JOIN weave_members member ON member.workspace_id=link.workspace_id AND member.user_id=link.user_id
    WHERE link.app_id=$1 AND link.open_id=$2 AND link.workspace_id=$3 AND link.user_id=$4 AND link.expires_at>statement_timestamp() AND NOT u.disabled`, app.appID, openID, *workspace, *user).Scan(&permissionJSON)
			if errors.Is(err, pgx.ErrNoRows) {
				result = "绑定已过期或员工当前无权办理，请在桌面重新登录并绑定。"
			} else if err != nil {
				return 0, err
			} else if json.Unmarshal(permissionJSON, &actor.Permissions) != nil {
				return 0, errors.New("feishu employee permissions invalid")
			} else if app.workspace == "" {
				var own bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_feishu_apps WHERE workspace_id=$1)`, *workspace).Scan(&own); err != nil {
					return 0, err
				}
				if own {
					result = "本工作区已改用自己的飞书应用，请在桌面重新绑定。"
				}
			}
		}
		if result == "" {
			if text == "解绑" {
				_, err = tx.Exec(ctx, `DELETE FROM weave_feishu_links WHERE app_id=$1 AND open_id=$2`, app.appID, openID)
				if err != nil {
					return 0, err
				}
				result = "已解绑。已接单的工作继续运行。"
			} else if text == "团队" {
				result, err = s.feishuTeams(ctx, actor)
				if err != nil {
					return 0, err
				}
			} else {
				// Freeze selection before any dispatch or human-resume side effect. A
				// crashed attempt never follows a changed publication or a newer task.
				prepared := command.Registration != nil || command.Human != nil
				if !prepared && (strings.HasPrefix(text, "开始 ") || strings.HasPrefix(text, "继续 ") || strings.HasPrefix(text, "确认 ")) {
					result, err = s.prepareFeishuCommand(ctx, actor, messageID, &command)
					if err != nil {
						return 0, err
					}
					if result == "" {
						frozen, _ := json.Marshal(command)
						_, err = tx.Exec(ctx, `UPDATE weave_feishu_messages SET command=$3::jsonb WHERE app_id=$1 AND message_id=$2`, app.appID, messageID, string(frozen))
						if err != nil {
							return 0, err
						}
						return 1, tx.Commit(ctx)
					}
				} else if command.Registration != nil {
					result, inputID, runID, err = s.executeFeishuRegistration(ctx, actor, *command.Registration)
				} else if command.Human != nil {
					_, err = s.invokeFeishu(ctx, actor, s.handleCompleteHumanTask, *command.Human, []string{"run_id"}, []string{command.HumanRunID})
					result = "已提交确认，团队继续运行。"
				} else if text == "查看" {
					result, err = s.feishuLatestResult(ctx, actor)
				} else {
					result = "发送“团队”“开始 团队名称 任务内容”“查看”“继续 补充要求”“确认 JSON”或“解绑”。"
				}
				var rejected *feishuRejected
				if errors.As(err, &rejected) {
					result = "当前工作无法办理。请查看最新状态；涉及业务授权、文件或正式审批时回到桌面处理。"
					if rejected.code == 422 && command.Human != nil {
						result = "确认内容不符合当前问题的格式，请按收到的格式重新提交。"
					}
					if rejected.code == 403 {
						result = "当前员工没有使用这个团队的权限。"
					}
					if rejected.code == 409 || rejected.code == 404 {
						result = "工作或问题已变化，请发送“查看”核对最新状态。"
					}
					err = nil
				}
				if err != nil {
					return 0, err
				}
			}
		}
	}
	_, err = tx.Exec(ctx, `UPDATE weave_feishu_messages SET response=$3,input_revision_id=NULLIF($4,''),run_id=NULLIF($5,'') WHERE app_id=$1 AND message_id=$2`, app.appID, messageID, result, inputID, runID)
	if err != nil {
		return 0, err
	}
	return 1, tx.Commit(ctx)
}

func (s *Server) feishuTeams(ctx context.Context, actor feishuActor) (string, error) {
	teams, err := s.OrgStore.ListBusinessTeams(ctx, actor.WorkspaceID)
	if err != nil {
		return "", err
	}
	names := []string{}
	for _, team := range teams {
		if team.Status != "active" {
			continue
		}
		workflow, err := s.feishuTeamWorkflow(ctx, actor.WorkspaceID, team.ID, team.DefaultWorkflowID)
		if err != nil {
			return "", err
		}
		if workflow == "" {
			continue
		}
		name := team.DisplayName
		if name == "" {
			name = team.Name
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return "暂无开启飞书接入的可用团队。", nil
	}
	return "可用团队：\n" + strings.Join(names, "\n"), nil
}

// feishuTeamWorkflow returns the workflow Feishu starts for a team, or "" when
// the team has not enabled Feishu access or its chosen workflow is unusable.
func (s *Server) feishuTeamWorkflow(ctx context.Context, workspace, team, defaultWorkflow string) (string, error) {
	access, err := s.feishuTeamAccess(ctx, s.GetPool(), workspace, team)
	if err != nil || !access.Enabled {
		return "", err
	}
	if access.WorkflowID == nil {
		return defaultWorkflow, nil
	}
	usable, err := s.feishuWorkflowUsable(ctx, workspace, team, *access.WorkflowID)
	if err != nil || !usable {
		return "", err
	}
	return *access.WorkflowID, nil
}

func (s *Server) latestFeishuInput(ctx context.Context, actor feishuActor) (dispatchInputRevision, error) {
	var inputID string
	err := s.GetPool().QueryRow(ctx, `SELECT input_revision_id FROM weave_feishu_messages WHERE app_id=$1 AND open_id=$2 AND workspace_id=$3 AND user_id=$4 AND run_id IS NOT NULL ORDER BY created_at DESC,message_id DESC LIMIT 1`, actor.App.appID, actor.OpenID, actor.WorkspaceID, actor.UserID).Scan(&inputID)
	if err != nil {
		return dispatchInputRevision{}, err
	}
	return s.loadDispatchInput(ctx, actor.WorkspaceID, actor.UserID, inputID)
}

func (s *Server) prepareFeishuCommand(ctx context.Context, actor feishuActor, messageID string, command *feishuCommand) (string, error) {
	text := strings.TrimSpace(command.Text)
	none := []string{}
	seq := int64(0)
	registration := dispatchInputRegistration{RegistrationID: feishuMessageKey(actor.App.appID, messageID), WorkbenchSessionID: "feishu:" + feishuMessageKey(actor.App.appID, messageID),
		ProjectID: workbenchProjectID(actor.UserID), SourceMessages: []dispatchInputSourceMessage{{MessageID: messageID, EventSeq: &seq, SHA256: dispatchInputDigest([]byte(command.Text))}}, AuthorizedBusinessCapabilityIDs: &none}
	if strings.HasPrefix(text, "开始 ") {
		teams, err := s.OrgStore.ListBusinessTeams(ctx, actor.WorkspaceID)
		if err != nil {
			return "", err
		}
		remainder := strings.TrimSpace(strings.TrimPrefix(text, "开始 "))
		matches := 0
		for _, team := range teams {
			name := team.DisplayName
			if name == "" {
				name = team.Name
			}
			if name == "" || !strings.HasPrefix(remainder, name+" ") {
				continue
			}
			workflow, err := s.feishuTeamWorkflow(ctx, actor.WorkspaceID, team.ID, team.DefaultWorkflowID)
			if err != nil {
				return "", err
			}
			if workflow == "" {
				continue
			}
			matches++
			registration.TeamID = team.ID
			registration.Task = strings.TrimSpace(strings.TrimPrefix(remainder, name))
			registration.WorkflowID = workflow
		}
		if matches != 1 || registration.Task == "" || registration.WorkflowID == "" {
			return "未找到唯一的团队和任务内容。请发送“团队”查看名称。", nil
		}
		wf, err := s.Workflow.Get(ctx, actor.WorkspaceID, registration.WorkflowID)
		if err != nil {
			return "", err
		}
		if wf.PublishedVersion == nil {
			return "团队暂无已发布流程。", nil
		}
		version := *wf.PublishedVersion
		registration.WorkflowVersion = &version
	} else {
		previous, err := s.latestFeishuInput(ctx, actor)
		if errors.Is(err, pgx.ErrNoRows) {
			return "暂无可继续的工作。请先发送“开始 团队名称 任务内容”。", nil
		}
		if err != nil {
			return "", err
		}
		if !previous.IsCurrent || previous.IsClosed {
			return "原工作已关闭或被新修订取代，请在桌面核对最新工作。", nil
		}
		registration.TeamID = previous.TeamID
		registration.WorkflowID = previous.WorkflowID
		version := previous.WorkflowVersion
		registration.WorkflowVersion = &version
		registration.WorkbenchSessionID = previous.WorkbenchSessionID
		registration.ExpectedRevisionID = previous.InputRevisionID
		if strings.HasPrefix(text, "确认 ") {
			var runStatus, waitKind string
			var waitRaw []byte
			var generation, resumeGeneration int64
			err = s.GetPool().QueryRow(ctx, `SELECT status,COALESCE(wait_kind,''),wait_detail,team_run_generation,resume_generation FROM weave_team_runs WHERE workspace_id=$1 AND run_id=$2`, actor.WorkspaceID, previous.ConsumedRunID).Scan(&runStatus, &waitKind, &waitRaw, &generation, &resumeGeneration)
			if err != nil {
				return "", err
			}
			if runStatus != "parked" || waitKind != "human" {
				return "当前工作没有等待确认的团队节点。", nil
			}
			human, result, err := prepareFeishuHuman(actor.WorkspaceID, previous, waitRaw, generation, resumeGeneration, strings.TrimSpace(strings.TrimPrefix(text, "确认 ")), registration.RegistrationID)
			if err != nil {
				return "", err
			}
			if result != "" {
				return result, nil
			}
			command.Human = human
			command.HumanRunID = previous.ConsumedRunID
			return "", nil
		}
		var status string
		var actions int
		err = s.GetPool().QueryRow(ctx, `SELECT run.status,(SELECT count(*) FROM weave_task_business_delegations d WHERE d.workspace_id=run.workspace_id AND d.input_revision_id=$3) FROM weave_team_runs run WHERE run.workspace_id=$1 AND run.run_id=$2`, actor.WorkspaceID, previous.ConsumedRunID, previous.InputRevisionID).Scan(&status, &actions)
		if err != nil {
			return "", err
		}
		if status != "succeeded" && status != "failed" {
			return "工作尚未结束，请查看当前进度；等待人工处理时发送“确认 JSON”。", nil
		}
		if actions != 0 {
			return "这项工作包含业务授权，请回到桌面继续。", nil
		}
		registration.Task = strings.TrimSpace(strings.TrimPrefix(text, "继续 "))
		registration.RevisionContext = &dispatchRevisionContext{ParentInputRevisionID: previous.InputRevisionID, ParentRunID: previous.ConsumedRunID}
	}
	command.Registration = &registration
	return "", nil
}

func (s *Server) executeFeishuRegistration(ctx context.Context, actor feishuActor, registration dispatchInputRegistration) (string, string, string, error) {
	data, err := s.invokeFeishu(ctx, actor, s.handleRegisterDispatchInput, registration, nil, nil)
	if err != nil {
		return "", "", "", err
	}
	var receipt dispatchInputReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.InputRevisionID == "" {
		return "", "", "", errors.New("feishu input receipt invalid")
	}
	data, err = s.invokeFeishu(ctx, actor, s.handleDispatchTeam, map[string]string{"input_revision_id": receipt.InputRevisionID}, []string{"id"}, []string{registration.TeamID})
	if err != nil {
		return "", "", "", err
	}
	var run workflowManualRunResponse
	if json.Unmarshal(data, &run) != nil || run.RunID == "" {
		return "", "", "", errors.New("feishu run receipt invalid")
	}
	return "已接单。发送“查看”读取进度与结果。", receipt.InputRevisionID, run.RunID, nil
}

func (s *Server) feishuLatestResult(ctx context.Context, actor feishuActor) (string, error) {
	input, err := s.latestFeishuInput(ctx, actor)
	if errors.Is(err, pgx.ErrNoRows) {
		return "暂无工作。", nil
	}
	if err != nil {
		return "", err
	}
	data, err := s.invokeFeishu(ctx, actor, s.handleGetWorkbenchRunContext, nil, []string{"id"}, []string{input.ConsumedRunID})
	if err != nil {
		return "", err
	}
	var view workbenchContextResponse
	if json.Unmarshal(data, &view) != nil {
		return "", errors.New("feishu result invalid")
	}
	labels := map[string]string{"queued": "等待执行", "running": "正在执行", "parked": "等待处理", "succeeded": "已完成", "failed": "执行失败", "cancelled": "已取消", "abandoned": "已放弃"}
	label := labels[view.Run.Status]
	if label == "" {
		label = "状态待核对"
	}
	result := "团队工作：" + label
	if view.Run.FinalResult != nil {
		result += "\n" + truncateRunes(view.Run.FinalResult.Content, 6000)
	}
	if view.Run.Status == "parked" {
		result += "\n团队人工节点可发送“确认 JSON”；业务事项请在桌面办理。"
	}
	return result, nil
}

type feishuResponseWriter struct {
	headers http.Header
	code    int
	body    bytes.Buffer
}

func (w *feishuResponseWriter) Header() http.Header            { return w.headers }
func (w *feishuResponseWriter) WriteHeader(code int)           { w.code = code }
func (w *feishuResponseWriter) Write(data []byte) (int, error) { return w.body.Write(data) }
