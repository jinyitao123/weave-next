package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	if kind != "business" {
		return workflowError(c, http.StatusUnprocessableEntity, "team_kind_unsupported", "这里只新建业务团队；代码团队请在管理端新建")
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
		if teamID, err = s.createStarterTeam(c, requestID.String(), name, objective); err != nil {
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

func (s *Server) starterAgent(ctx context.Context, ws, name, displayName, role, model, prompt string) (string, error) {
	if existing, err := s.Registry.Get(ctx, ws, name); err == nil && existing != nil {
		return existing.ID, nil
	}
	record := registry.AgentRecord{Name: name, DisplayName: displayName, Role: role, Engine: "loom", Model: model, GraphType: "standard"}
	record.Spec.SystemPrompt = prompt
	if err := s.Registry.Put(ctx, ws, &record); err != nil {
		return "", err
	}
	return record.ID, nil
}

// createStarterTeam builds the team under names derived from the request id,
// so a repeated request finds what an earlier attempt left behind.
func (s *Server) createStarterTeam(c echo.Context, suffix, name, objective string) (string, error) {
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
	lead, err := s.starterAgent(ctx, ws, "team-"+suffix+"-lead", starterLeadName, "avatar", model, fmt.Sprintf("负责理解“%s”的目标并汇总可核验结果。", name))
	if err != nil {
		return "", err
	}
	worker, err := s.starterAgent(ctx, ws, "team-"+suffix+"-handle", starterWorkerName, "worker", model, starterWorkerPrompt)
	if err != nil {
		return "", err
	}
	var teamID string
	result, err := s.OrgStore.CreateActiveTeam(ctx, ws, org.CreateActiveTeamInput{
		Name: "team-" + suffix, DisplayName: name, Objective: objective, PrimaryScenario: objective,
		SuccessCriteria: "按任务要求交付结果并说明依据", LeadAvatarID: lead,
		Workers: []org.InitialTeamWorker{{
			WorkerAgentID: worker, Duty: starterWorkerPrompt, WhenToUse: "负责人分配相关工作时", ContextInstruction: "保留本次任务的原话和材料来源。",
			AllowedKinds: []string{"consult", "dispatch", "handoff"}, DefaultKind: "dispatch", ResultRequirement: starterWorkerRequires,
		}},
	})
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
	operations := []map[string]any{
		{"kind": "member", "member": starterLeadName, "duty": starterLeadDuty},
		{"kind": "flow_add", "name": starterFlowName, "description": objective, "member": starterWorkerName},
	}
	if _, err = applyDevelopmentOperations(&draft.Document, operations, nil); err != nil {
		return "", err
	}
	if err = validateDevelopmentDocument(draft.Document); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$3,revision=revision+1,updated_by=$4,updated_at=now() WHERE workspace_id=$1 AND team_id=$2`, ws, teamID, encodeDevelopment(draft.Document), getUserID(c)); err != nil {
		return "", err
	}
	return teamID, tx.Commit(ctx)
}
