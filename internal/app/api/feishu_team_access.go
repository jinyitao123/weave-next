package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v4"
)

// Which run event kinds reach the Feishu sender of a team's work. Kinds a team
// turns off are settled as suppressed; the Forge inbox is never affected.
type feishuNotify struct {
	Result           bool `json:"result"`
	RevisionRequired bool `json:"revisionRequired"`
	HumanReview      bool `json:"humanReview"`
	Failure          bool `json:"failure"`
	Cancelled        bool `json:"cancelled"`
}

func (n feishuNotify) allows(kind string) bool {
	switch kind {
	case "result":
		return n.Result
	case "revision_required":
		return n.RevisionRequired
	case "human_review":
		return n.HumanReview
	case "failure":
		return n.Failure
	case "cancelled":
		return n.Cancelled
	}
	return true
}

type feishuTeamAccess struct {
	Enabled    bool         `json:"enabled"`
	WorkflowID *string      `json:"workflowId"`
	Notify     feishuNotify `json:"notify"`
	Revision   int64        `json:"revision"`
	UpdatedAt  *time.Time   `json:"updatedAt,omitempty"`
	UpdatedBy  string       `json:"updatedBy,omitempty"`
}

// A team without a row is not reachable from Feishu.
func defaultFeishuTeamAccess() feishuTeamAccess {
	return feishuTeamAccess{Notify: feishuNotify{true, true, true, true, true}}
}

func (s *Server) feishuTeamAccess(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, workspace, team string) (feishuTeamAccess, error) {
	access := defaultFeishuTeamAccess()
	var notify []byte
	var updatedAt time.Time
	err := q.QueryRow(ctx, `SELECT access.enabled,access.workflow_id,access.notify,access.revision,access.updated_at,COALESCE(NULLIF(u.display_name,''),u.username,'')
 FROM weave_feishu_team_access access LEFT JOIN weave_users u ON u.id=access.updated_by
 WHERE access.workspace_id=$1 AND access.team_id=$2`, workspace, team).Scan(&access.Enabled, &access.WorkflowID, &notify, &access.Revision, &updatedAt, &access.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return access, nil
	}
	if err != nil {
		return access, err
	}
	access.UpdatedAt = &updatedAt
	if json.Unmarshal(notify, &access.Notify) != nil {
		return access, errors.New("feishu team notify settings invalid")
	}
	return access, nil
}

func (s *Server) teamExists(ctx context.Context, workspace, team string) (bool, error) {
	var exists bool
	err := s.GetPool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_teams WHERE workspace_id=$1 AND id=$2)`, workspace, team).Scan(&exists)
	return exists, err
}

// feishuWorkflowUsable reports whether a workflow belongs to the team and has a
// published version Feishu can start.
func (s *Server) feishuWorkflowUsable(ctx context.Context, workspace, team, workflow string) (bool, error) {
	var usable bool
	err := s.GetPool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_team_workflows WHERE workspace_id=$1 AND team_id=$2 AND id=$3
 AND status<>'archived' AND published_version IS NOT NULL)`, workspace, team, workflow).Scan(&usable)
	return usable, err
}

func (s *Server) handleGetFeishuTeamAccess(c echo.Context) error {
	if s.GetPool() == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	ctx, workspace, team := c.Request().Context(), getTenant(c), c.Param("id")
	exists, err := s.teamExists(ctx, workspace, team)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if !exists {
		return feishuError(c, http.StatusNotFound, "团队不存在")
	}
	access, err := s.feishuTeamAccess(ctx, s.GetPool(), workspace, team)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	return c.JSON(http.StatusOK, access)
}

func (s *Server) handlePutFeishuTeamAccess(c echo.Context) error {
	pool := s.GetPool()
	if pool == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	var input struct {
		ExpectedRevision int64        `json:"expectedRevision"`
		Enabled          bool         `json:"enabled"`
		WorkflowID       *string      `json:"workflowId"`
		Notify           feishuNotify `json:"notify"`
	}
	if err := c.Bind(&input); err != nil || input.ExpectedRevision < 0 {
		return feishuError(c, http.StatusBadRequest, "请求格式不正确")
	}
	if input.WorkflowID != nil {
		if trimmed := strings.TrimSpace(*input.WorkflowID); trimmed == "" {
			input.WorkflowID = nil
		} else {
			input.WorkflowID = &trimmed
		}
	}
	ctx, workspace, team := c.Request().Context(), getTenant(c), c.Param("id")
	exists, err := s.teamExists(ctx, workspace, team)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if !exists {
		return feishuError(c, http.StatusNotFound, "团队不存在")
	}
	if input.WorkflowID != nil {
		usable, err := s.feishuWorkflowUsable(ctx, workspace, team, *input.WorkflowID)
		if err != nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		if !usable {
			return feishuError(c, http.StatusUnprocessableEntity, "所选流程不属于本团队或尚未发布")
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	defer tx.Rollback(ctx)
	var before []byte
	var revision int64
	err = tx.QueryRow(ctx, `SELECT jsonb_build_object('enabled',enabled,'workflowId',workflow_id,'notify',notify),revision
 FROM weave_feishu_team_access WHERE workspace_id=$1 AND team_id=$2 FOR UPDATE`, workspace, team).Scan(&before, &revision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if input.ExpectedRevision != revision {
		return feishuError(c, http.StatusConflict, "飞书接入设置已被修改，请刷新后再保存")
	}
	notify, _ := json.Marshal(input.Notify)
	after, _ := json.Marshal(map[string]any{"enabled": input.Enabled, "workflowId": input.WorkflowID, "notify": input.Notify})
	actor := getUserID(c)
	revision++
	// A first save cannot lock a missing row, so a concurrent first save loses
	// on the primary key instead of being overwritten.
	query := `UPDATE weave_feishu_team_access SET enabled=$3,workflow_id=$4,notify=$5::jsonb,revision=$6,updated_by=$7,updated_at=now()
 WHERE workspace_id=$1 AND team_id=$2 AND revision=$6-1`
	if revision == 1 {
		query = `INSERT INTO weave_feishu_team_access(workspace_id,team_id,enabled,workflow_id,notify,revision,updated_by)
 VALUES($1,$2,$3,$4,$5::jsonb,$6,$7) ON CONFLICT(workspace_id,team_id) DO NOTHING`
	}
	tag, err := tx.Exec(ctx, query, workspace, team, input.Enabled, input.WorkflowID, string(notify), revision, actor)
	if err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if tag.RowsAffected() != 1 {
		return feishuError(c, http.StatusConflict, "飞书接入设置已被修改，请刷新后再保存")
	}
	var previous any
	if before != nil {
		previous = string(before)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO weave_feishu_team_access_audit(workspace_id,team_id,actor,revision,before,after) VALUES($1,$2,$3,$4,$5::jsonb,$6::jsonb)`,
		workspace, team, actor, revision, previous, string(after)); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	if err = tx.Commit(ctx); err != nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}
	return s.handleGetFeishuTeamAccess(c)
}
