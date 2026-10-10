package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

// developmentIssue is one thing still missing from a draft. Blocks says what
// it stops: "trial", "publish", or "none" for advice that stops nothing.
type developmentIssue struct {
	Scope    string `json:"scope"`
	Target   string `json:"target"`
	Workflow string `json:"workflow,omitempty"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Blocks   string `json:"blocks"`
}

type developmentOperationsResult struct {
	Revision int64              `json:"revision"`
	Saved    bool               `json:"saved"`
	Changes  []string           `json:"changes"`
	Issues   []developmentIssue `json:"issues"`
}

// readBusinessCapabilities returns the workspace's catalog snapshot; known is
// false when no developer sign-in has captured one yet.
func readBusinessCapabilities(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workspace string) (capabilities []catalogCapability, known bool, err error) {
	var raw []byte
	err = query.QueryRow(ctx, `SELECT capabilities FROM weave_business_capability_catalog WHERE workspace_id=$1`, workspace).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return []catalogCapability{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err = json.Unmarshal(raw, &capabilities); err != nil {
		return nil, false, err
	}
	return capabilities, true, nil
}

// developmentIssues lists what a draft still lacks. The console and the
// desktop assistant both show this list instead of working it out themselves.
func developmentIssues(doc developmentDocument, catalog []catalogCapability, catalogKnown bool) []developmentIssue {
	issues := []developmentIssue{}
	add := func(scope, target, workflow, code, blocks, message string) {
		issues = append(issues, developmentIssue{Scope: scope, Target: target, Workflow: workflow, Code: code, Message: message, Blocks: blocks})
	}
	if strings.TrimSpace(doc.Objective) == "" {
		add("team", doc.Name, "", "objective_missing", "none", "团队目标为空，员工在桌面查不到这个团队")
	}
	members := map[string]bool{}
	for _, member := range doc.Members {
		cfg, name := member.Configuration, member.Configuration.DisplayName
		members[member.ID] = true
		cli := engine.IsCLIEngine(cfg.Engine)
		if strings.TrimSpace(cfg.SystemPrompt) == "" || strings.TrimSpace(member.Relationship.Duty) == "" {
			add("member", name, "", "member_incomplete", "trial", "请填写职责和工作方法")
		}
		if !cli && !(cfg.Role == "avatar" && cfg.Engine == "loom") && strings.TrimSpace(cfg.Model) == "" {
			add("member", name, "", "member_model_missing", "trial", "内置引擎成员需要选择模型")
		}
		if cfg.Engine != "loom" && !cli || len(cfg.MCPServerIDs) > 0 || len(cfg.SkillNames) > 0 || len(cfg.PermissionAllow) > 0 || len(cfg.PermissionAsk) > 0 {
			add("member", name, "", "member_tools_unsupported", "trial", "试跑只支持不带外部工具的成员")
		}
		for _, id := range cfg.BusinessCapabilityIDs {
			if action := editCapability(catalog, id); catalogKnown && (action == nil || action.Status != "available" || action.ExecutionMode == "employee_only") {
				add("member", name, "", "business_action_unavailable", "trial", "绑定的一项业务动作已不在目录中或暂不可用，请重新选择")
			}
		}
	}
	if len(doc.Workflows) == 0 {
		add("team", doc.Name, "", "workflow_missing", "trial", "团队还没有流程")
	}
	for _, flow := range doc.Workflows {
		if strings.TrimSpace(flow.Description) == "" {
			add("workflow", flow.Name, flow.Name, "workflow_description_missing", "none", "流程说明为空，桌面无法判断这个团队接什么、需要什么、交付什么")
		}
		graph, err := decodeEditGraph(flow.Graph)
		if err != nil {
			add("workflow", flow.Name, flow.Name, "workflow_invalid", "trial", "流程定义无法读取")
			continue
		}
		if _, report := machine.DecodeGraphDefinitionV1(flow.Graph); report != nil && len(report.Issues) > 0 {
			add("workflow", flow.Name, flow.Name, "workflow_invalid", "trial", "流程检查未通过："+report.Issues[0].Message)
		}
		for _, node := range editItems(graph, "nodes") {
			if editNodeType(node) == "worker" && !members[editString(editObject(node["config"])["agent_id"])] {
				add("step", editString(node["label"]), flow.Name, "step_member_missing", "trial", "步骤引用了已移出的成员，请重新选择执行者")
			}
		}
		if issue := editResultProtocolIssue(graph); issue != "" {
			add("workflow", flow.Name, flow.Name, "result_protocol_invalid", "trial", issue)
		}
		acts := false
		for _, member := range editFlowMembers(&doc, graph) {
			acts = acts || len(member.Configuration.BusinessCapabilityIDs) > 0
		}
		completion, err := editCompletionRequirement(graph)
		switch {
		case err != nil:
			add("workflow", flow.Name, flow.Name, "business_completion_invalid", "publish", err.Error())
		case completion == nil && acts:
			add("workflow", flow.Name, flow.Name, "business_completion_missing", "publish", "成员带有业务动作，交付步骤还没有选定必须办成的业务动作")
		case completion != nil && catalogKnown:
			if err = editRequireCompletionBindings(&doc, graph, completion.Capabilities, catalog); err == nil {
				_, err = editRequireCompletionOutput(graph)
			}
			if err != nil {
				add("workflow", flow.Name, flow.Name, "business_completion_invalid", "publish", err.Error())
			}
		}
	}
	return issues
}

func developmentOperationsDigest(revision int64, operations []map[string]any) (string, error) {
	raw, err := json.Marshal(map[string]any{"expected_revision": revision, "operations": operations})
	if err != nil {
		return "", err
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// handleApplyTeamDevelopmentOperations applies a group of controlled edits to
// the draft, or only reports what they would change when dry_run is set. A
// saved group is remembered by its request id, so a caller that lost the
// response learns the outcome by sending the same request again.
func (s *Server) handleApplyTeamDevelopmentOperations(c echo.Context) error {
	if s.Pool == nil {
		return echo.NewHTTPError(503, "开发服务暂不可用")
	}
	var request struct {
		Revision   int64            `json:"expected_revision"`
		RequestID  string           `json:"request_id"`
		DryRun     bool             `json:"dry_run"`
		Operations []map[string]any `json:"operations"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	requestID, err := uuid.Parse(request.RequestID)
	if err != nil || len(request.Operations) < 1 || len(request.Operations) > 24 {
		return workflowError(c, http.StatusUnprocessableEntity, "operation_invalid", "须提供有效的 request_id 和 1 至 24 项修改")
	}
	digest, err := developmentOperationsDigest(request.Revision, request.Operations)
	if err != nil {
		return workflowSchemaError(c)
	}
	ctx, ws, id := c.Request().Context(), getTenant(c), c.Param("id")
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d, err := readDevelopment(ctx, tx, ws, id, !request.DryRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowError(c, http.StatusNotFound, "development_not_found", "团队不存在，或还没有读取过开发草稿")
	}
	if err != nil {
		return err
	}
	catalog, catalogKnown, err := readBusinessCapabilities(ctx, tx, ws)
	if err != nil {
		return err
	}
	if !request.DryRun {
		var storedDigest string
		var stored developmentOperationsResult
		var changes []byte
		err = tx.QueryRow(ctx, `SELECT digest,revision_after,changes FROM weave_team_development_requests WHERE workspace_id=$1 AND team_id=$2 AND request_id=$3`, ws, id, requestID).Scan(&storedDigest, &stored.Revision, &changes)
		if err == nil {
			if storedDigest != digest {
				return workflowError(c, http.StatusConflict, "request_conflict", "这个请求标识已用于另一组修改")
			}
			if err = json.Unmarshal(changes, &stored.Changes); err != nil {
				return err
			}
			stored.Saved, stored.Issues = true, developmentIssues(d.Document, catalog, catalogKnown)
			return c.JSON(http.StatusOK, stored)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if d.Revision != request.Revision || d.PublishingRevision != 0 {
		return workflowError(c, http.StatusConflict, "development_revision_stale", "草稿已变化或正在发布，请重新读取后再提交")
	}
	changes, err := applyDevelopmentOperations(&d.Document, request.Operations, catalog)
	var refusal *editError
	if errors.As(err, &refusal) {
		return c.JSON(http.StatusUnprocessableEntity, map[string]any{"code": "operation_invalid", "error": refusal.Message, "index": refusal.Index})
	}
	if err != nil {
		return err
	}
	if err = validateDevelopmentDocument(d.Document); err != nil {
		return err
	}
	result := developmentOperationsResult{Revision: d.Revision, Changes: changes, Issues: developmentIssues(d.Document, catalog, catalogKnown)}
	if request.DryRun {
		return c.JSON(http.StatusOK, result)
	}
	kinds := map[string]int{}
	for _, operation := range request.Operations {
		kinds[editString(operation["kind"])]++
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$4,revision=revision+1,updated_by=$5,updated_at=now() WHERE workspace_id=$1 AND team_id=$2 AND revision=$3 AND publishing_revision=0`, ws, id, request.Revision, encodeDevelopment(d.Document), getUserID(c))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return workflowError(c, http.StatusConflict, "development_revision_stale", "草稿已变化或正在发布，请重新读取后再提交")
	}
	result.Revision, result.Saved = d.Revision+1, true
	if _, err = tx.Exec(ctx, `INSERT INTO weave_team_development_requests(workspace_id,team_id,request_id,digest,actor_id,revision_before,revision_after,kinds,changes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		ws, id, requestID, digest, getUserID(c), d.Revision, result.Revision, encodeDevelopment(kinds), encodeDevelopment(changes)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) handleListDevelopmentOperations(c echo.Context) error {
	topics := make([]map[string]string, len(developmentOperationTopics))
	for i, topic := range developmentOperationTopics {
		topics[i] = map[string]string{"topic": topic.Topic, "title": topic.Title, "covers": topic.Covers}
	}
	return c.JSON(http.StatusOK, map[string]any{"version": developmentOperationsVersion, "topics": topics, "console_only": developmentConsoleOnly})
}

func (s *Server) handleGetDevelopmentOperationTopic(c echo.Context) error {
	for _, topic := range developmentOperationTopics {
		if topic.Topic == c.Param("topic") {
			return c.JSON(http.StatusOK, topic)
		}
	}
	return workflowError(c, http.StatusNotFound, "operation_topic_not_found", "没有这一类修改")
}
