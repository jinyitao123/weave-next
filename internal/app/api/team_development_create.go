package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	org "github.com/jinyitao123/weave/internal/kernel/orgspec"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// A new business team starts with a lead, one member and the plain flow: the
// lead reads the task, the member handles it, the member's result is delivered.
const (
	starterWorkerName     = "业务办理员"
	starterWorkerPrompt   = "只依据本次任务的原话和材料办理。把事实、估计和待确认事项分开写；没有给出的数字和日期不要推测。"
	starterWorkerRequires = "说明办理结果、依据和待确认项。"
	starterLeadName       = "团队负责人"
	starterLeadDuty       = "理解任务目标并汇总可核验结果"
	starterFlowName       = "业务流程"
)

// A new code team starts like the one the console makes: one member codes,
// another on a different engine verifies, and work that fails verification
// goes back to coding, at most codeStarterRounds times.
type codeStarterMember struct{ key, name, engine, prompt, requires string }

var codeStarterMembers = []codeStarterMember{
	{"code", "编码", "claude", "分析需求、设计方案并修改代码。收到验证意见时先逐条修复。说明改了什么、为什么这样改，以及还没完成的部分。", "完成代码修改，说明改动内容、依据和未完成项。"},
	{"verify", "验证", "codex", "独立验证上一步的代码：运行测试和检查，逐条记录执行的命令与结果，不根据描述推断结果。", "列出执行的检查命令和真实结果。全部通过时 passed 为 true；否则为 false，并在 summary 中写明未通过项和原因。"},
}

const (
	codeStarterFlowName = "开发与验证"
	codeStarterRounds   = 3
)

type developmentTeamCreated struct {
	TeamID   string `json:"team_id"`
	Name     string `json:"name"`
	Revision int64  `json:"revision"`
}

// handleCreateDevelopmentTeam creates a starter team once per request id. A
// caller that lost the response sends the same request again and gets the same
// team; whatever an interrupted attempt already created is picked up, not
// created twice.
func (s *Server) handleCreateDevelopmentTeam(c echo.Context) error {
	if s.Pool == nil || s.Registry == nil || s.OrgStore == nil {
		return echo.NewHTTPError(503, "开发服务暂不可用")
	}
	var request struct {
		RequestID string `json:"request_id"`
		Name      string `json:"name"`
		Objective string `json:"objective"`
		Kind      string `json:"kind"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	requestID, err := uuid.Parse(request.RequestID)
	name, objective := strings.TrimSpace(request.Name), strings.TrimSpace(request.Objective)
	if err != nil || name == "" || utf8.RuneCountInString(name) > 80 || objective == "" || utf8.RuneCountInString(objective) > 2000 {
		return workflowError(c, http.StatusUnprocessableEntity, "team_create_invalid", "须提供有效的 request_id、团队名称（80 字以内）和目标（2000 字以内）")
	}
	kind := request.Kind
	if kind == "" {
		kind = "business"
	}
	if kind != "business" && kind != "code" {
		return workflowError(c, http.StatusUnprocessableEntity, "team_kind_unsupported", "团队类型只能是业务团队（business）或代码团队（code）")
	}
	sum := sha256.Sum256([]byte(name + "\x00" + objective + "\x00" + kind))
	digest := hex.EncodeToString(sum[:])

	ctx, ws := c.Request().Context(), getTenant(c)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO weave_team_development_creations(workspace_id,request_id,digest,actor_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, ws, requestID, digest, getUserID(c)); err != nil {
		return err
	}
	var stored, teamID string
	if err = tx.QueryRow(ctx, `SELECT digest,COALESCE(team_id,'') FROM weave_team_development_creations WHERE workspace_id=$1 AND request_id=$2 FOR UPDATE`, ws, requestID).Scan(&stored, &teamID); err != nil {
		return err
	}
	if stored != digest {
		return workflowError(c, http.StatusConflict, "request_conflict", "这个请求标识已用于另一个团队")
	}
	status := http.StatusOK
	if teamID == "" {
		if teamID, err = s.createStarterTeam(c, requestID.String(), name, objective, kind); err != nil {
			var failure *echo.HTTPError
			if errors.As(err, &failure) {
				return err
			}
			return workflowError(c, http.StatusConflict, "team_create_failed", "团队没有建成，请用同一请求重试")
		}
		if _, err = tx.Exec(ctx, `UPDATE weave_team_development_creations SET team_id=$3 WHERE workspace_id=$1 AND request_id=$2`, ws, requestID, teamID); err != nil {
			return err
		}
		status = http.StatusCreated
	}
	created := developmentTeamCreated{TeamID: teamID}
	if err = tx.QueryRow(ctx, `SELECT revision,document->>'name' FROM weave_team_development_drafts WHERE workspace_id=$1 AND team_id=$2`, ws, teamID).Scan(&created.Revision, &created.Name); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return c.JSON(status, created)
}

func (s *Server) starterAgent(ctx context.Context, ws, name, displayName, role, engine, model, prompt string) (string, error) {
	if existing, err := s.Registry.Get(ctx, ws, name); err == nil && existing != nil {
		return existing.ID, nil
	}
	record := registry.AgentRecord{Name: name, DisplayName: displayName, Role: role, Engine: engine, Model: model, GraphType: "standard"}
	record.Spec.SystemPrompt = prompt
	if err := s.Registry.Put(ctx, ws, &record); err != nil {
		return "", err
	}
	return record.ID, nil
}

// createStarterTeam builds the team under names derived from the request id,
// so a repeated request finds what an earlier attempt left behind.
func (s *Server) createStarterTeam(c echo.Context, suffix, name, objective, kind string) (string, error) {
	ctx, ws := c.Request().Context(), getTenant(c)
	// The lead takes the first step of the starting flow, so it needs a model
	// as much as the member does. A workspace with none enabled gets a team
	// whose draft says both members still need one; no stand-in is invented.
	models, err := s.developmentModels(ctx, ws)
	if err != nil {
		return "", err
	}
	model := ""
	if len(models) > 0 {
		model = models[0]
	}
	lead, err := s.starterAgent(ctx, ws, "team-"+suffix+"-lead", starterLeadName, "avatar", "loom", model, fmt.Sprintf("负责理解“%s”的目标并汇总可核验结果。", name))
	if err != nil {
		return "", err
	}
	input := org.CreateActiveTeamInput{
		Name: "team-" + suffix, DisplayName: name, Objective: objective, PrimaryScenario: objective,
		SuccessCriteria: "按任务要求交付结果并说明依据", LeadAvatarID: lead,
	}
	if kind == "code" {
		input.SuccessCriteria = "交付经过独立验证的代码改动"
		for _, member := range codeStarterMembers {
			// A member on an external engine uses that engine's own model.
			worker, err := s.starterAgent(ctx, ws, "team-"+suffix+"-"+member.key, member.name, "worker", member.engine, "", member.prompt)
			if err != nil {
				return "", err
			}
			input.Workers = append(input.Workers, org.InitialTeamWorker{
				WorkerAgentID: worker, Duty: member.prompt, WhenToUse: "负责人分配相关工作时", ContextInstruction: "保留任务上下文、代码版本和材料来源。",
				AllowedKinds: []string{"consult", "dispatch", "handoff"}, DefaultKind: "dispatch", ResultRequirement: member.requires,
			})
		}
	} else {
		worker, err := s.starterAgent(ctx, ws, "team-"+suffix+"-handle", starterWorkerName, "worker", "loom", model, starterWorkerPrompt)
		if err != nil {
			return "", err
		}
		input.Workers = []org.InitialTeamWorker{{
			WorkerAgentID: worker, Duty: starterWorkerPrompt, WhenToUse: "负责人分配相关工作时", ContextInstruction: "保留本次任务的原话和材料来源。",
			AllowedKinds: []string{"consult", "dispatch", "handoff"}, DefaultKind: "dispatch", ResultRequirement: starterWorkerRequires,
		}}
	}
	var teamID string
	result, err := s.OrgStore.CreateActiveTeam(ctx, ws, input)
	switch {
	case err == nil:
		teamID = result.Team.ID
	case errors.Is(err, org.ErrTeamNameConflict), errors.Is(err, org.ErrTeamLeadConflict):
		if err = s.Pool.QueryRow(ctx, `SELECT id FROM weave_teams WHERE workspace_id=$1 AND name=$2 AND status IN ('active','building')`, ws, "team-"+suffix).Scan(&teamID); err != nil {
			return "", err
		}
	default:
		return "", err
	}

	names, values := c.ParamNames(), c.ParamValues()
	defer func() { c.SetParamNames(names...); c.SetParamValues(values...) }()
	c.SetParamNames("id")
	c.SetParamValues(teamID)
	if err = s.ensureDevelopmentDraft(c); err != nil {
		return "", err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	draft, err := readDevelopment(ctx, tx, ws, teamID, true)
	if err != nil {
		return "", err
	}
	if len(draft.Document.Workflows) > 0 {
		return teamID, nil
	}
	if kind == "code" {
		if _, err = applyDevelopmentOperations(&draft.Document, []map[string]any{{"kind": "member", "member": starterLeadName, "duty": starterLeadDuty}}, nil); err != nil {
			return "", err
		}
		flow, err := codeStarterWorkflow(draft.Document, objective)
		if err != nil {
			return "", err
		}
		draft.Document.Workflows = append(draft.Document.Workflows, flow)
	} else {
		operations := []map[string]any{
			{"kind": "member", "member": starterLeadName, "duty": starterLeadDuty},
			{"kind": "flow_add", "name": starterFlowName, "description": objective, "member": starterWorkerName},
		}
		if _, err = applyDevelopmentOperations(&draft.Document, operations, nil); err != nil {
			return "", err
		}
	}
	if err = validateDevelopmentDocument(draft.Document); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$3,revision=revision+1,updated_by=$4,updated_at=now() WHERE workspace_id=$1 AND team_id=$2`, ws, teamID, encodeDevelopment(draft.Document), getUserID(c)); err != nil {
		return "", err
	}
	return teamID, tx.Commit(ctx)
}

// codeStarterWorkflow is the console's starting code flow: a bounded loop whose
// body is coding then verification; verification answers {passed, summary},
// coding sees the previous round's summary, and the last summary is delivered.
func codeStarterWorkflow(document developmentDocument, objective string) (developmentWorkflow, error) {
	ids := map[string]string{}
	for _, member := range document.Members {
		for _, starter := range codeStarterMembers {
			if member.Configuration.Role == "worker" && member.Configuration.DisplayName == starter.name {
				ids[starter.key] = member.ID
			}
		}
	}
	if ids["code"] == "" || ids["verify"] == "" {
		return developmentWorkflow{}, errors.New("code team starter members are missing from the draft")
	}
	codeStep, verifyStep, loop := "step-"+uuid.NewString(), "step-"+uuid.NewString(), "loop-"+uuid.NewString()
	original := map[string]any{"value": map[string]any{"source": "run_input", "path": ""}, "expected_type": "text"}
	edge := func(from, to, route string) map[string]any {
		return map[string]any{"id": uuid.NewString(), "from_node_id": from, "to_node_id": to, "route": route}
	}
	worker := func(id, label, agent, requires string, inputs map[string]any, output map[string]any) map[string]any {
		return map[string]any{"id": id, "type": "worker", "label": label, "inputs": inputs, "output": output,
			"config": map[string]any{"kind": "consult", "agent_id": agent, "agent_version": 1, "result_requirement": requires}}
	}
	graph := map[string]any{
		"schema_version": 1, "entry_node_id": loop,
		"input_contract": map[string]any{"type": "text"}, "output_contract": map[string]any{"type": "text"},
		"nodes": []any{
			map[string]any{"id": loop, "type": "loop", "label": "验证未通过时退回", "config": map[string]any{
				"max_iterations": codeStarterRounds, "latch_node_id": verifyStep,
				"continue_predicate": map[string]any{
					"left":     map[string]any{"source": "node_output", "node_id": verifyStep, "path": "/passed", "iteration": "current_iteration"},
					"operator": "eq", "right": map[string]any{"source": "literal", "value": false},
				},
			}},
			worker(codeStep, codeStarterMembers[0].name, ids["code"], codeStarterMembers[0].requires, map[string]any{
				"original": original,
				"review": map[string]any{"value": map[string]any{"source": "node_output", "node_id": verifyStep, "path": "/summary", "iteration": "previous_iteration",
					"default": map[string]any{"source": "literal", "value": ""}}, "expected_type": "text"},
			}, map[string]any{"type": "text"}),
			worker(verifyStep, codeStarterMembers[1].name, ids["verify"], codeStarterMembers[1].requires, map[string]any{
				"original": original,
				"previous": map[string]any{"value": map[string]any{"source": "node_output", "node_id": codeStep, "path": "", "iteration": "current_iteration"}, "expected_type": "text"},
			}, map[string]any{"type": "json", "schema": map[string]any{
				"type": "object", "additionalProperties": false, "required": []any{"passed", "summary"},
				"properties": map[string]any{"passed": map[string]any{"type": "boolean"}, "summary": map[string]any{"type": "string"}},
			}}),
			map[string]any{"id": "deliver", "type": "deliver", "label": "交付结果", "config": map[string]any{
				"result": map[string]any{"source": "node_output", "node_id": loop, "path": "/latch_result/summary"}}},
		},
		"edges": []any{edge(loop, codeStep, "body"), edge(codeStep, verifyStep, "success"), edge(verifyStep, loop, "back"), edge(loop, "deliver", "exit")},
	}
	encoded, err := json.Marshal(graph)
	if err != nil {
		return developmentWorkflow{}, err
	}
	return developmentWorkflow{
		ID: uuid.NewString(), Name: codeStarterFlowName, Description: objective, Graph: encoded,
		Trigger: json.RawMessage(`{"schema_version":1,"type":"conversation_explicit","config":{}}`),
	}, nil
}
