package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/jinyitao123/weave/internal/app/teamconstruction"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

type agentExecutionRequest struct {
	ExpectedVersion int      `json:"expected_version"`
	Engine          string   `json:"engine"`
	RuntimeID       string   `json:"runtime_id"`
	Model           string   `json:"model"`
	FallbackModels  []string `json:"fallback_models"`
	FallbackRetries int      `json:"fallback_retries"`
}

// Runtime administration exposes execution fields without agent prompts or secrets.
func (s *Server) handleListAgentExecutionSettings(c echo.Context) error {
	if s.Registry == nil {
		return workflowError(c, 503, "workflow_store_unavailable", "执行配置服务不可用")
	}
	records, err := s.Registry.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	items := make([]map[string]any, 0, len(records))
	for _, record := range records {
		if record.Visibility == "platform" || strings.HasPrefix(record.Name, "_") {
			continue
		}
		items = append(items, map[string]any{"name": record.Name, "display_name": record.DisplayName, "version": record.Version, "engine": record.Engine, "runtime_id": record.RuntimeID, "model": record.Model, "fallback_models": record.FallbackModels, "fallback_retries": record.FallbackRetries})
	}
	return c.JSON(200, map[string]any{"agents": items})
}

// A member edit and every current workflow that refers to it publish together.
// Existing runs keep their frozen bundles; a failed preflight changes nothing.
func (s *Server) handleConfigureAgentExecution(c echo.Context) error {
	if s.Registry == nil || s.Workflow == nil || s.ScheduleTransactions == nil || s.Runtimes == nil ||
		s.Pool == nil || s.ProductPublication == nil || s.PublicationAuthority == nil {
		return workflowError(c, 503, "workflow_store_unavailable", "执行配置服务不可用")
	}
	var request agentExecutionRequest
	if err := decodeWorkflowBody(c, &request); err != nil || request.ExpectedVersion < 1 || !engine.IsCLIEngine(request.Engine) || strings.TrimSpace(request.RuntimeID) == "" || len(request.Model) > 200 || len(request.FallbackModels) > 2 || request.FallbackRetries < 0 || request.FallbackRetries > 2 {
		return workflowSchemaError(c)
	}
	request.Model = strings.TrimSpace(request.Model)
	for _, model := range request.FallbackModels {
		if strings.TrimSpace(model) == "" || len(model) > 200 {
			return workflowSchemaError(c)
		}
	}
	ctx, workspaceID := c.Request().Context(), getTenant(c)
	requestID, requestDigest, subject, err := newAgentExecutionRequestIdentity(ctx, workspaceID, c.Param("name"), request)
	if err != nil {
		return mapAgentExecutionRequestError(c, err)
	}
	if prepared, found, err := findAgentExecutionPlan(ctx, s.Pool, workspaceID, requestID, requestDigest, subject); err != nil {
		return mapAgentExecutionRequestError(c, err)
	} else if found {
		return s.finishAgentExecutionPlan(c, prepared)
	}
	record, err := s.Registry.Get(ctx, workspaceID, c.Param("name"))
	if err != nil {
		return c.JSON(404, map[string]string{"error": "智能体不存在"})
	}
	if record.Visibility == "platform" || strings.HasPrefix(record.Name, "_") {
		return c.JSON(403, map[string]string{"error": "平台智能体不提供成员执行配置"})
	}
	if record.Engine == request.Engine && record.RuntimeID == request.RuntimeID && record.Model == request.Model && slices.Equal(record.FallbackModels, request.FallbackModels) && record.FallbackRetries == request.FallbackRetries {
		return c.JSON(200, map[string]any{"version": record.Version, "published_workflows": []any{}, "changed": false})
	}
	runtime, err := s.Runtimes.Get(ctx, workspaceID, request.RuntimeID)
	if err != nil || !runtime.Enabled || !slices.Contains(runtime.Engines, request.Engine) {
		return c.JSON(422, map[string]string{"error": "所选运行节点未提供这个引擎"})
	}
	if record.Version != request.ExpectedVersion {
		return c.JSON(409, map[string]string{"error": "智能体配置已改变，请刷新后再保存"})
	}
	record.Engine, record.RuntimeID, record.Model = request.Engine, request.RuntimeID, request.Model
	record.FallbackModels = append([]string(nil), request.FallbackModels...)
	record.FallbackRetries = request.FallbackRetries
	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, requestID); err != nil {
		return workflowStoreFailure(c, err)
	}
	if prepared, found, err := loadAgentExecutionPlan(ctx, tx, workspaceID, requestID, requestDigest, subject); err != nil {
		return mapAgentExecutionRequestError(c, err)
	} else if found {
		_ = tx.Rollback(ctx)
		return s.finishAgentExecutionPlan(c, prepared)
	}
	if err := s.Registry.PutTx(ctx, tx, workspaceID, record); err != nil {
		return agentWritePutErrorResponse(c, err)
	}
	if record.Version != request.ExpectedVersion+1 {
		return c.JSON(409, map[string]string{"error": "智能体配置已改变，请刷新后再保存"})
	}
	rows, err := tx.Query(ctx, `SELECT w.id FROM weave_team_workflows w WHERE w.workspace_id=$1 AND w.status<>'archived' AND w.published_version IS NOT NULL AND EXISTS (SELECT 1 FROM weave_team_workflow_dependencies d WHERE d.workspace_id=w.workspace_id AND d.workflow_id=w.id AND d.workflow_version=w.published_version AND d.owner_type='agent' AND d.owner_id=$2) ORDER BY w.id`, workspaceID, record.ID)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	var workflows []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return workflowStoreFailure(c, err)
		}
		workflows = append(workflows, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return workflowStoreFailure(c, err)
	}
	commands := make([]teamconstruction.PublicationCommand, 0, len(workflows))
	actor := getUserID(c)
	if actor == "" {
		actor = "workbench"
	}
	for _, id := range workflows {
		draft, err := s.Workflow.CreateDraftTx(ctx, tx, workspaceID, id, actor)
		if errors.Is(err, workflow.ErrDraftExists) {
			return c.JSON(409, map[string]string{"error": "关联工作流有未发布修改，请先处理该草稿后再调整执行配置"})
		}
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		var graph any
		if err := json.Unmarshal(draft.GraphDefinition, &graph); err != nil {
			return workflowStoreFailure(c, err)
		}
		replaceExecutionAgentVersion(graph, record.ID, record.Version)
		encoded, err := json.Marshal(graph)
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		draft, err = s.Workflow.UpdateDraftTx(ctx, tx, workspaceID, id, draft.Version, draft.UpdatedAt, workflow.DraftInput{TriggerConfig: draft.TriggerConfig, GraphDefinition: encoded, CreatedBy: actor})
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		candidate, report, err := s.PublicationAuthority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: workspaceID, WorkflowID: id, WorkflowVersion: draft.Version})
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		if candidate == nil || report != nil && len(report.Issues) > 0 {
			return c.JSON(422, map[string]any{"error": "更新后的工作流未通过发布检查，配置未保存", "code": "workflow_candidate_invalid", "issues": report})
		}
		childRequestID := requestID + ":" + id + ":" + candidate.ContentHash
		command, err := teamconstruction.PublicationCommandForCandidate(childRequestID, candidate, teamconstruction.PublicationTarget{
			TeamID: candidate.Payload.Team.TeamID,
		})
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		if _, err := s.ProductPublication.ReserveTx(ctx, tx, command); err != nil {
			return mapWorkflowPublishError(c, err)
		}
		commands = append(commands, command)
	}
	plan := agentExecutionPublicationPlan{Subject: subject, RequestID: requestID, RequestDigest: requestDigest,
		AgentName: record.Name, Request: request, AgentVersion: record.Version, PublicationCommands: commands}
	if err := insertAgentExecutionPlan(ctx, tx, plan); err != nil {
		return workflowStoreFailure(c, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowStoreFailure(c, err)
	}
	return s.finishAgentExecutionPlan(c, plan)
}

func mapAgentExecutionRequestError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, execution.ErrSubjectRequired):
		return c.JSON(http.StatusUnauthorized, map[string]string{"error": "需要已登录的操作身份"})
	case errors.Is(err, execution.ErrSubjectMismatch), errors.Is(err, publication.ErrRequestConflict):
		return c.JSON(http.StatusConflict, map[string]string{"error": "这次配置请求与已保存的操作记录不一致"})
	default:
		return workflowStoreFailure(c, err)
	}
}

func (s *Server) finishAgentExecutionPlan(c echo.Context, plan agentExecutionPublicationPlan) error {
	if plan.State == "completed" {
		return c.JSONBlob(http.StatusOK, plan.Response)
	}
	completed := make(map[string]struct{}, len(plan.CompletedPublications))
	for _, requestID := range plan.CompletedPublications {
		completed[requestID] = struct{}{}
	}
	published := make([]map[string]any, 0, len(plan.PublicationCommands))
	for _, command := range plan.PublicationCommands {
		requestID := command.Request.RequestID
		if _, ok := completed[requestID]; !ok {
			if _, err := s.ProductPublication.Publish(c.Request().Context(), command); err != nil {
				return mapWorkflowPublishError(c, err)
			}
			if err := recordAgentExecutionPublication(c.Request().Context(), s.Pool, plan, requestID); err != nil {
				return workflowStoreFailure(c, err)
			}
		}
		published = append(published, map[string]any{
			"workflow_id": command.Request.Candidate.WorkflowID,
			"version":     command.Request.Candidate.WorkflowVersion,
		})
	}
	response, err := json.Marshal(map[string]any{
		"version": plan.AgentVersion, "published_workflows": published, "changed": true,
	})
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if err := completeAgentExecutionPlan(c.Request().Context(), s.Pool, plan, response); err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSONBlob(http.StatusOK, response)
}

func replaceExecutionAgentVersion(value any, agentID string, version int) {
	switch item := value.(type) {
	case map[string]any:
		if item["agent_id"] == agentID {
			if _, present := item["agent_version"]; present {
				item["agent_version"] = version
			}
		}
		for _, child := range item {
			replaceExecutionAgentVersion(child, agentID, version)
		}
	case []any:
		for _, child := range item {
			replaceExecutionAgentVersion(child, agentID, version)
		}
	}
}
