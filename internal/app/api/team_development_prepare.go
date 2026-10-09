package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

// Preparation creates immutable member versions and an exact candidate without
// moving any existing active member or workflow head.
func (s *Server) prepareDevelopment(ctx context.Context, ws, id, actor string, revision int64) (developmentDraft, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return developmentDraft{}, err
	}
	defer tx.Rollback(ctx)
	d, err := readDevelopment(ctx, tx, ws, id, true)
	if err != nil {
		return d, err
	}
	if d.Revision != revision {
		return d, developmentError("草稿已经变化，请重新试跑")
	}
	if err = checkDevelopmentBaseline(ctx, tx, ws, id, d.Baseline); err != nil {
		return d, err
	}
	if d.PreparedRevision == revision && len(d.Prepared) > 0 {
		if d.PreparedActor != actor {
			return d, developmentError("这份草稿已由其他开发者试跑，请保存自己的修改后重新试跑")
		}
		return d, nil
	}
	if err = validateDevelopmentDocument(d.Document); err != nil {
		return d, err
	}
	// A frozen runtime binding is identified by node and revision but carries
	// the member's engine, so one node can serve CLI members of one engine only
	// within a publication. Report that before compilation fails opaquely.
	nodeEngines := map[string]teamMemberAgentConfiguration{}
	for _, m := range d.Document.Members {
		cfg := m.Configuration
		runtimeID := strings.TrimSpace(cfg.RuntimeID)
		if !engine.IsCLIEngine(cfg.Engine) || runtimeID == "" {
			continue
		}
		if other, exists := nodeEngines[runtimeID]; exists && other.Engine != cfg.Engine {
			return d, echo.NewHTTPError(422, fmt.Sprintf("“%s”和“%s”使用不同引擎，请把它们分配到不同节点", other.DisplayName, cfg.DisplayName))
		}
		nodeEngines[runtimeID] = cfg
	}
	team := registry.PublicationTeamRead{WorkspaceID: ws, TeamID: id, Status: "active", Workers: []registry.TeamWorker{}}
	members := map[string]registry.AgentRecord{}
	for _, m := range d.Document.Members {
		cfg := m.Configuration
		// CLI members (Claude, Codex, OpenCode) run on registered runtime nodes
		// with the node's own engine login, so they need no model selection;
		// built-in Loom members still bind an explicit provider model.
		cliMember := engine.IsCLIEngine(cfg.Engine)
		// A Loom lead without a model is providerless: it borrows the model of the
		// run's CLI runtime (runtimeDefaultForProviderlessLoom) and is never a
		// step of a serial CLI workflow.
		providerlessLead := cfg.Role == "avatar" && cfg.Engine == "loom"
		if strings.TrimSpace(cfg.SystemPrompt) == "" || strings.TrimSpace(m.Relationship.Duty) == "" || (!cliMember && !providerlessLead && strings.TrimSpace(cfg.Model) == "") {
			return d, echo.NewHTTPError(422, "请为每位成员填写职责和工作方法；内置引擎成员还需选择模型")
		}
		// Forge business actions are supplied to candidate trials through the
		// isolated development dispatcher. Other external resources still require
		// their own sandbox binding; never borrow production credentials or silently
		// remove those capabilities.
		// No Weave-held external resource is lent to a trial: CLI members use the
		// node's own engine login and carry no MCP servers, skills or extra tool
		// permissions here.
		if (cfg.Engine != "loom" && !cliMember) || len(cfg.MCPServerIDs) > 0 || len(cfg.SkillNames) > 0 || len(cfg.PermissionAllow) > 0 || len(cfg.PermissionAsk) > 0 {
			return d, echo.NewHTTPError(422, "当前试跑支持无外部工具的成员；此团队需要先配置隔离的测试工具环境")
		}
		var rec registry.AgentRecord
		if _, exists := d.Baseline.Members[m.ID]; exists {
			var raw []byte
			if err = tx.QueryRow(ctx, `SELECT spec FROM weave_agents WHERE workspace_id=$1 AND id=$2`, ws, m.ID).Scan(&raw); err != nil {
				return d, err
			}
			if err = json.Unmarshal(raw, &rec); err != nil {
				return d, err
			}
			if rec.Role != cfg.Role {
				return d, echo.NewHTTPError(422, "已有成员身份不能直接切换")
			}
		} else {
			if cfg.Role == "avatar" {
				return d, echo.NewHTTPError(422, "请保留现有负责人")
			}
			rec = registry.AgentRecord{Name: "development-" + id + "-" + m.ID, Role: "worker", Engine: "loom"}
		}
		skills, e := inlineSkills(cfg.Skills)
		if e != nil {
			return d, e
		}
		if err = configureDevelopmentMember(&rec, cfg, m.Relationship, cfg.Role == "avatar", skills); err != nil {
			return d, err
		}
		if err = s.Registry.StageTx(ctx, tx, ws, &rec); err != nil {
			return d, err
		}
		members[m.ID] = rec
		if cfg.Role == "avatar" {
			team.LeadAvatarID = rec.ID
			team.LeadAvatarVersion = int64(rec.Version)
		} else {
			rel := m.Relationship
			if len(rel.AllowedKinds) == 0 {
				rel.AllowedKinds = []string{"consult", "dispatch"}
				rel.DefaultKind = "dispatch"
			}
			if err = validateAppliedRelationship(rel); err != nil {
				return d, err
			}
			team.Workers = append(team.Workers, registry.TeamWorker{WorkspaceID: ws, TeamID: id, WorkerAgentID: rec.ID, Duty: rel.Duty, WhenToUse: rel.WhenToUse, ContextInstruction: rel.ContextInstruction, AllowedKinds: rel.AllowedKinds, DefaultKind: rel.DefaultKind, ResultRequirement: rel.ResultRequirement, Enabled: rel.Enabled})
		}
	}
	prepared := []developmentPrepared{}
	for _, f := range d.Document.Workflows {
		var owner string
		err = tx.QueryRow(ctx, `SELECT team_id FROM weave_team_workflows WHERE workspace_id=$1 AND id=$2 FOR UPDATE`, ws, f.ID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err = tx.Exec(ctx, `INSERT INTO weave_team_workflows(workspace_id,id,team_id,name,description) VALUES($1,$2,$3,$4,$5)`, ws, f.ID, id, f.Name, f.Description); err != nil {
				return d, err
			}
		} else if err != nil {
			return d, err
		} else if owner != id {
			return d, echo.NewHTTPError(403, "流程不属于当前团队")
		}
		var version int
		err = tx.QueryRow(ctx, `SELECT version FROM weave_team_workflow_versions WHERE workspace_id=$1 AND workflow_id=$2 AND status='draft' FOR UPDATE`, ws, f.ID).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM weave_team_workflow_versions WHERE workspace_id=$1 AND workflow_id=$2`, ws, f.ID).Scan(&version); err != nil {
				return d, err
			}
		} else if err != nil {
			return d, err
		}
		graph, err := resolveDevelopmentGraph(f.Graph, members)
		if err != nil {
			return d, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO weave_team_workflow_versions(workspace_id,workflow_id,version,trigger_config,graph_definition,created_by) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,workflow_id,version) DO UPDATE SET trigger_config=EXCLUDED.trigger_config,graph_definition=EXCLUDED.graph_definition,updated_at=now() WHERE weave_team_workflow_versions.status='draft'`, ws, f.ID, version, string(f.Trigger), string(graph), actor); err != nil {
			return d, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO weave_team_development_candidates(workspace_id,workflow_id,workflow_version,team_id,revision,team_read) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(workspace_id,workflow_id,workflow_version) DO UPDATE SET revision=EXCLUDED.revision,team_read=EXCLUDED.team_read`, ws, f.ID, version, id, revision, encodeDevelopment(team)); err != nil {
			return d, err
		}
		candidate, report, err := s.PublicationAuthority.BuildCandidateTx(ctx, tx, workflow.CandidateInput{WorkspaceID: ws, WorkflowID: f.ID, WorkflowVersion: version})
		if err != nil {
			return d, err
		}
		if report != nil && len(report.Issues) > 0 {
			return d, echo.NewHTTPError(http.StatusUnprocessableEntity, fmt.Sprintf("流程“%s”检查未通过：%s", f.Name, report.Issues[0].Message))
		}
		envelope, err := workflow.CandidateEnvelope(candidate)
		if err != nil {
			return d, err
		}
		prepared = append(prepared, developmentPrepared{ID: f.ID, Envelope: envelope, Members: members})
	}
	if len(prepared) == 0 {
		return d, echo.NewHTTPError(422, "请保留至少一条可执行流程")
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_development_drafts SET prepared_revision=$3,prepared=$4,prepared_actor=$5 WHERE workspace_id=$1 AND team_id=$2`, ws, id, revision, encodeDevelopment(prepared), actor); err != nil {
		return d, err
	}
	if err = tx.Commit(ctx); err != nil {
		return d, err
	}
	d.Prepared = prepared
	d.PreparedRevision = revision
	d.PreparedActor = actor
	return d, nil
}
func resolveDevelopmentGraph(raw json.RawMessage, members map[string]registry.AgentRecord) (json.RawMessage, error) {
	var graph map[string]any
	if err := json.Unmarshal(raw, &graph); err != nil {
		return nil, err
	}
	nodes, ok := graph["nodes"].([]any)
	if !ok {
		return nil, echo.NewHTTPError(422, "流程没有步骤")
	}
	for _, item := range nodes {
		node, ok := item.(map[string]any)
		if !ok {
			return nil, echo.NewHTTPError(422, "步骤无效")
		}
		if node["type"] != "worker" {
			continue
		}
		cfg, ok := node["config"].(map[string]any)
		if !ok {
			return nil, echo.NewHTTPError(422, "步骤缺少成员")
		}
		key, _ := cfg["agent_id"].(string)
		member, ok := members[key]
		if !ok {
			for _, m := range members {
				if m.ID == key {
					member = m
					ok = true
					break
				}
			}
		}
		if !ok || member.Role != "worker" {
			return nil, echo.NewHTTPError(422, "步骤引用了已移出的成员，请先调整流程")
		}
		cfg["agent_id"], cfg["agent_version"] = member.ID, member.Version
	}
	return json.Marshal(graph)
}

// activateDevelopment applies the tested collection of members and workflows
// together. The Kernel has already issued durable, verified publication receipts.
func (s *Server) activateDevelopment(ctx context.Context, ws, id, actor string, d developmentDraft) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	current, err := readDevelopment(ctx, tx, ws, id, true)
	if err != nil {
		return err
	}
	if current.Revision != d.Revision {
		return developmentError("试跑后草稿已修改，请重新试跑")
	}
	if current.PublishedRevision == d.Revision {
		return nil
	}
	if err = checkDevelopmentBaseline(ctx, tx, ws, id, d.Baseline); err != nil {
		return err
	}
	members := d.Prepared[0].Members
	ids := []string{}
	var leadID string
	for key, rec := range members {
		if _, err = tx.Exec(ctx, `UPDATE weave_agents SET display_name=$3,spec=$4,version=$5,updated_at=now() WHERE workspace_id=$1 AND id=$2`, ws, rec.ID, rec.DisplayName, encodeDevelopment(rec), rec.Version); err != nil {
			return err
		}
		if rec.Role == "avatar" {
			leadID = rec.ID
			continue
		}
		ids = append(ids, rec.ID)
		var rel teamMemberRelationshipDraft
		for _, m := range d.Document.Members {
			if m.ID == key {
				rel = m.Relationship
			}
		}
		if len(rel.AllowedKinds) == 0 {
			rel.AllowedKinds = []string{"consult", "dispatch"}
			rel.DefaultKind = "dispatch"
		}
		_, err = tx.Exec(ctx, `INSERT INTO weave_team_workers(workspace_id,team_id,worker_agent_id,duty,when_to_use,context_instruction,allowed_kinds,default_kind,result_requirement,enabled) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(team_id,worker_agent_id) DO UPDATE SET duty=EXCLUDED.duty,when_to_use=EXCLUDED.when_to_use,context_instruction=EXCLUDED.context_instruction,allowed_kinds=EXCLUDED.allowed_kinds,default_kind=EXCLUDED.default_kind,result_requirement=EXCLUDED.result_requirement,enabled=EXCLUDED.enabled,updated_at=now()`, ws, id, rec.ID, rel.Duty, rel.WhenToUse, rel.ContextInstruction, rel.AllowedKinds, rel.DefaultKind, rel.ResultRequirement, rel.Enabled)
		if err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `DELETE FROM weave_team_workers WHERE workspace_id=$1 AND team_id=$2 AND NOT(worker_agent_id=ANY($3::text[]))`, ws, id, ids); err != nil {
		return err
	}
	flowIDs := []string{}
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, p := range d.Prepared {
		flowIDs = append(flowIDs, p.ID)
		if _, err = tx.Exec(ctx, `UPDATE weave_team_workflow_versions SET status='published',published_at=$4,updated_at=$4 WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3 AND status='draft'`, ws, p.ID, p.Envelope.WorkflowVersion, now); err != nil {
			return err
		}
		var name, description string
		for _, f := range d.Document.Workflows {
			if f.ID == p.ID {
				name = f.Name
				description = f.Description
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE weave_team_workflows SET published_version=$3,name=$4,description=$5,status='active',updated_at=$6 WHERE workspace_id=$1 AND id=$2`, ws, p.ID, p.Envelope.WorkflowVersion, name, description, now); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_workflows SET status='archived',updated_at=$4 WHERE workspace_id=$1 AND team_id=$2 AND NOT(id=ANY($3::text[])) AND status='active'`, ws, id, flowIDs, now); err != nil {
		return err
	}
	audience, err := normalizeTeamAudience(d.Document.Audience)
	if err != nil {
		return err
	}
	audienceJSON, _ := json.Marshal(audience)
	if _, err = tx.Exec(ctx, `UPDATE weave_teams SET display_name=$3,objective=$4,lead_avatar_id=$5,default_workflow_id=$6,updated_at=$7,audience=$8::jsonb WHERE workspace_id=$1 AND id=$2`, ws, id, d.Document.Name, d.Document.Objective, leadID, flowIDs[0], now, string(audienceJSON)); err != nil {
		return err
	}
	var teamUpdatedAt time.Time
	if err = tx.QueryRow(ctx, `SELECT updated_at FROM weave_teams WHERE workspace_id=$1 AND id=$2`, ws, id).Scan(&teamUpdatedAt); err != nil {
		return err
	}
	base := developmentBaseline{TeamUpdatedAt: teamUpdatedAt, Members: map[string]int{}, Workflows: map[string]time.Time{}}
	for i, m := range d.Document.Members {
		rec := members[m.ID]
		d.Document.Members[i].ID = rec.ID
		base.Members[rec.ID] = rec.Version
	}
	for i, f := range d.Document.Workflows {
		graph, err := resolveDevelopmentGraph(f.Graph, members)
		if err != nil {
			return err
		}
		d.Document.Workflows[i].Graph = graph
		base.Workflows[f.ID] = now
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_development_drafts SET published_revision=$3,publishing_revision=0,document=$4,published_document=$4,baseline=$5,updated_by=$6,updated_at=now() WHERE workspace_id=$1 AND team_id=$2`, ws, id, d.Revision, encodeDevelopment(d.Document), encodeDevelopment(base), actor); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
