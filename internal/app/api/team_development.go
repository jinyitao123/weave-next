package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

type developmentMember struct {
	ID            string                       `json:"id"`
	Configuration teamMemberAgentConfiguration `json:"configuration"`
	Relationship  teamMemberRelationshipDraft  `json:"relationship"`
}
type developmentWorkflow struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Graph       json.RawMessage `json:"graph_definition"`
	Trigger     json.RawMessage `json:"trigger_config"`
}
type developmentDocument struct {
	Name      string `json:"name"`
	Objective string `json:"objective"`
	// Audience lists the Forge permission sets that may use the team; empty
	// means the whole organization. Publishing freezes it (decision 002).
	Audience  []string              `json:"audience"`
	Members   []developmentMember   `json:"members"`
	Workflows []developmentWorkflow `json:"workflows"`
}
type developmentBaseline struct {
	TeamUpdatedAt time.Time            `json:"team_updated_at"`
	Members       map[string]int       `json:"members"`
	Workflows     map[string]time.Time `json:"workflows"`
}
type developmentPrepared struct {
	ID       string                          `json:"id"`
	Envelope frozen.ArtifactEnvelopeV1       `json:"envelope"`
	Members  map[string]registry.AgentRecord `json:"members"`
}
type developmentDraft struct {
	PublishedDocument  developmentDocument    `json:"published_document"`
	PreparedActor      string                 `json:"-"`
	Revision           int64                  `json:"revision"`
	PublishingRevision int64                  `json:"publishing_revision"`
	PublishedRevision  int64                  `json:"published_revision"`
	Document           developmentDocument    `json:"document"`
	Baseline           developmentBaseline    `json:"-"`
	PreparedRevision   int64                  `json:"prepared_revision"`
	Prepared           []developmentPrepared  `json:"-"`
	UpdatedAt          time.Time              `json:"updated_at"`
	Trials             []developmentTrialView `json:"trials"`
}
type developmentTrialView struct {
	RequestID  string    `json:"request_id"`
	Revision   int64     `json:"revision"`
	WorkflowID string    `json:"workflow_id"`
	RunID      string    `json:"run_id"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

func developmentError(message string) error { return echo.NewHTTPError(http.StatusConflict, message) }
func encodeDevelopment(value any) string    { raw, _ := json.Marshal(value); return string(raw) }

func (s *Server) seedDevelopment(c echo.Context) (developmentDocument, developmentBaseline, error) {
	ctx, ws, teamID := c.Request().Context(), getTenant(c), c.Param("id")
	doc := developmentDocument{Audience: []string{}, Members: []developmentMember{}, Workflows: []developmentWorkflow{}}
	base := developmentBaseline{Members: map[string]int{}, Workflows: map[string]time.Time{}}
	var leadID string
	var audienceJSON []byte
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(NULLIF(display_name,''),name),objective,updated_at,lead_avatar_id,audience FROM weave_teams WHERE workspace_id=$1 AND id=$2 AND status IN ('active','building')`, ws, teamID).Scan(&doc.Name, &doc.Objective, &base.TeamUpdatedAt, &leadID, &audienceJSON)
	if err != nil {
		return doc, base, err
	}
	if err := json.Unmarshal(audienceJSON, &doc.Audience); err != nil {
		return doc, base, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT id FROM weave_agents WHERE workspace_id=$1 AND (id=$2 OR id IN (SELECT worker_agent_id FROM weave_team_workers WHERE workspace_id=$1 AND team_id=$3)) ORDER BY CASE WHEN id=$2 THEN 0 ELSE 1 END,id`, ws, leadID, teamID)
	if err != nil {
		return doc, base, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return doc, base, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	names, values := c.ParamNames(), c.ParamValues()
	defer func() { c.SetParamNames(names...); c.SetParamValues(values...) }()
	for _, id := range ids {
		c.SetParamNames("id", "agent")
		c.SetParamValues(teamID, id)
		member, err := s.seedTeamMemberConfigDraft(c)
		if err != nil {
			return doc, base, err
		}
		base.Members[id] = member.BaseAgentVersion
		doc.Members = append(doc.Members, developmentMember{ID: id, Configuration: member.Configuration, Relationship: member.Relationship})
	}
	rows, err = s.Pool.Query(ctx, `SELECT w.id,w.name,w.description,w.updated_at,v.graph_definition,v.trigger_config FROM weave_team_workflows w JOIN LATERAL (SELECT graph_definition,trigger_config FROM weave_team_workflow_versions WHERE workspace_id=w.workspace_id AND workflow_id=w.id ORDER BY CASE WHEN version=w.published_version THEN 0 ELSE 1 END,version DESC LIMIT 1) v ON true WHERE w.workspace_id=$1 AND w.team_id=$2 AND w.status='active' ORDER BY w.created_at`, ws, teamID)
	if err != nil {
		return doc, base, err
	}
	defer rows.Close()
	for rows.Next() {
		var f developmentWorkflow
		var at time.Time
		if err = rows.Scan(&f.ID, &f.Name, &f.Description, &at, &f.Graph, &f.Trigger); err != nil {
			return doc, base, err
		}
		base.Workflows[f.ID] = at
		doc.Workflows = append(doc.Workflows, f)
	}
	return doc, base, rows.Err()
}
func readDevelopment(ctx context.Context, tx pgx.Tx, ws, teamID string, lock bool) (developmentDraft, error) {
	var d developmentDraft
	var doc, published, base, prepared []byte
	query := `SELECT revision,published_revision,publishing_revision,document,published_document,baseline,prepared_actor,prepared_revision,prepared,updated_at FROM weave_team_development_drafts WHERE workspace_id=$1 AND team_id=$2`
	if lock {
		query += " FOR UPDATE"
	}
	err := tx.QueryRow(ctx, query, ws, teamID).Scan(&d.Revision, &d.PublishedRevision, &d.PublishingRevision, &doc, &published, &base, &d.PreparedActor, &d.PreparedRevision, &prepared, &d.UpdatedAt)
	if err != nil {
		return d, err
	}
	for _, x := range []struct {
		raw    []byte
		target any
	}{{doc, &d.Document}, {published, &d.PublishedDocument}, {base, &d.Baseline}, {prepared, &d.Prepared}} {
		if err = json.Unmarshal(x.raw, x.target); err != nil {
			return d, err
		}
	}
	d.Trials = []developmentTrialView{}
	return d, nil
}
func (s *Server) handleGetTeamDevelopment(c echo.Context) error {
	if s.Pool == nil {
		return echo.NewHTTPError(503, "开发服务暂不可用")
	}
	ctx, ws, id := c.Request().Context(), getTenant(c), c.Param("id")
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_team_development_drafts WHERE workspace_id=$1 AND team_id=$2)`, ws, id).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		doc, base, err := s.seedDevelopment(c)
		if err != nil {
			return err
		}
		_, err = s.Pool.Exec(ctx, `INSERT INTO weave_team_development_drafts(workspace_id,team_id,document,published_document,baseline,updated_by) VALUES($1,$2,$3,$3,$4,$5) ON CONFLICT DO NOTHING`, ws, id, encodeDevelopment(doc), encodeDevelopment(base), getUserID(c))
		if err != nil {
			return err
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d, err := readDevelopment(ctx, tx, ws, id, false)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT t.request_id::text,t.revision,t.workflow_id,COALESCE(t.receipt->>'run_id',''),COALESCE(r.status,'submitting'),t.created_at FROM weave_team_development_trials t LEFT JOIN weave_team_runs r ON r.workspace_id=t.workspace_id AND r.run_id=t.receipt->>'run_id' WHERE t.workspace_id=$1 AND t.team_id=$2 AND t.actor_id=$3 ORDER BY t.created_at DESC LIMIT 30`, ws, id, getUserID(c))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v developmentTrialView
		if err = rows.Scan(&v.RequestID, &v.Revision, &v.WorkflowID, &v.RunID, &v.Status, &v.CreatedAt); err != nil {
			return err
		}
		d.Trials = append(d.Trials, v)
	}
	return c.JSON(http.StatusOK, d)
}
func validateDevelopmentDocument(doc developmentDocument) error {
	if strings.TrimSpace(doc.Name) == "" || len(doc.Name) > 240 || len(doc.Objective) > 30000 || len(doc.Members) < 2 || len(doc.Members) > 32 || len(doc.Workflows) > 20 {
		return echo.NewHTTPError(422, "请填写团队名称，并保留负责人和至少一位成员")
	}
	if _, err := normalizeTeamAudience(doc.Audience); err != nil {
		return echo.NewHTTPError(422, "可用人群须为不重复的 Forge 权限集名称，最多 32 项")
	}
	seen := map[string]bool{}
	leads := 0
	for _, m := range doc.Members {
		if _, err := uuid.Parse(m.ID); err != nil {
			return echo.NewHTTPError(422, "成员标识无效")
		}
		if seen[m.ID] || strings.TrimSpace(m.Configuration.DisplayName) == "" {
			return echo.NewHTTPError(422, "成员名称不能为空或重复")
		}
		seen[m.ID] = true
		if m.Configuration.Role == "avatar" {
			leads++
		} else if m.Configuration.Role != "worker" {
			return echo.NewHTTPError(422, "成员身份无效")
		}
	}
	if leads != 1 {
		return echo.NewHTTPError(422, "团队需要且只能有一位负责人")
	}
	seen = map[string]bool{}
	for _, f := range doc.Workflows {
		if _, err := uuid.Parse(f.ID); err != nil {
			return echo.NewHTTPError(422, "流程标识无效")
		}
		if seen[f.ID] || strings.TrimSpace(f.Name) == "" || !json.Valid(f.Graph) || !json.Valid(f.Trigger) {
			return echo.NewHTTPError(422, "流程名称或定义无效")
		}
		seen[f.ID] = true
	}
	return nil
}
func (s *Server) handleSaveTeamDevelopment(c echo.Context) error {
	var request struct {
		Revision int64               `json:"expected_revision"`
		Document developmentDocument `json:"document"`
	}
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowSchemaError(c)
	}
	if err := validateDevelopmentDocument(request.Document); err != nil {
		return err
	}
	ctx, ws, id := c.Request().Context(), getTenant(c), c.Param("id")
	tag, err := s.Pool.Exec(ctx, `UPDATE weave_team_development_drafts SET document=$4,revision=revision+1,updated_by=$5,updated_at=now() WHERE workspace_id=$1 AND team_id=$2 AND revision=$3 AND publishing_revision=0`, ws, id, request.Revision, encodeDevelopment(request.Document), getUserID(c))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return developmentError("草稿已变化或正在发布，请重新读取；未完成的发布可重试")
	}
	return s.handleGetTeamDevelopment(c)
}
func checkDevelopmentBaseline(ctx context.Context, tx pgx.Tx, ws, id string, base developmentBaseline) error {
	var updated time.Time
	if err := tx.QueryRow(ctx, `SELECT updated_at FROM weave_teams WHERE workspace_id=$1 AND id=$2 AND status IN ('active','building') FOR UPDATE`, ws, id).Scan(&updated); err != nil {
		return err
	}
	if !updated.Equal(base.TeamUpdatedAt) {
		return developmentError("线上团队已变化，请重新读取并合并修改")
	}
	for agent, want := range base.Members {
		var version int
		if err := tx.QueryRow(ctx, `SELECT version FROM weave_agents WHERE workspace_id=$1 AND id=$2 AND NOT deleted FOR UPDATE`, ws, agent).Scan(&version); err != nil {
			return err
		}
		if version != want {
			return developmentError("线上成员配置已变化，请合并后重试")
		}
	}
	for flow, want := range base.Workflows {
		if err := tx.QueryRow(ctx, `SELECT updated_at FROM weave_team_workflows WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, ws, flow).Scan(&updated); err != nil {
			return err
		}
		if !updated.Equal(want) {
			return developmentError("线上流程已变化，请合并后重试")
		}
	}
	return nil
}
func (s *Server) handleArchiveDevelopmentTeam(c echo.Context) error {
	ctx, ws, id := c.Request().Context(), getTenant(c), c.Param("id")
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var publishing int64
	err = tx.QueryRow(ctx, `SELECT publishing_revision FROM weave_team_development_drafts WHERE workspace_id=$1 AND team_id=$2 FOR UPDATE`, ws, id).Scan(&publishing)
	if err != nil && err != pgx.ErrNoRows {
		return err
	}
	if publishing != 0 {
		return developmentError("请先完成待恢复的发布，再删除团队")
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_workflows SET status='archived',updated_at=now() WHERE workspace_id=$1 AND team_id=$2 AND status<>'archived'`, ws, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE weave_teams SET status='archived',updated_at=now() WHERE workspace_id=$1 AND id=$2`, ws, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return echo.NewHTTPError(404, "团队不存在")
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	return c.NoContent(204)
}
