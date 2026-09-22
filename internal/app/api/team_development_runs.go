package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/labstack/echo/v4"
)

type developmentTrialRequest struct {
	Revision   int64  `json:"revision"`
	WorkflowID string `json:"workflow_id"`
	RequestID  string `json:"request_id"`
	Input      string `json:"input"`
}

func (s *Server) handleTrialTeamDevelopment(c echo.Context) error {
	var req developmentTrialRequest
	if err := decodeWorkflowBody(c, &req); err != nil {
		return workflowSchemaError(c)
	}
	if _, err := uuid.Parse(req.RequestID); err != nil || len(req.Input) == 0 || len(req.Input) > 700000 {
		return echo.NewHTTPError(422, "请提供测试材料及有效的试跑请求")
	}
	if s.KernelPublication == nil || s.PublicationAuthority == nil {
		return echo.NewHTTPError(503, "试跑服务暂不可用")
	}
	ctx, ws, id, actor := c.Request().Context(), getTenant(c), c.Param("id"), getUserID(c)
	d, err := s.prepareDevelopment(ctx, ws, id, actor, req.Revision)
	if err != nil {
		return err
	}
	var selected *developmentPrepared
	for i := range d.Prepared {
		if d.Prepared[i].ID == req.WorkflowID {
			selected = &d.Prepared[i]
		}
	}
	if selected == nil {
		return echo.NewHTTPError(422, "请先选择要试跑的流程")
	}
	input, _ := json.Marshal(req.Input)
	hash := sha256.Sum256(input)
	request := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "development:" + req.RequestID, Candidate: selected.Envelope, Input: input, InputVersion: hex.EncodeToString(hash[:]), SourceRef: "team-development:" + id, Purpose: "developer-trial"}
	digest, err := request.Fingerprint(ctx)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO weave_team_development_trials(workspace_id,team_id,request_id,revision,workflow_id,actor_id,request_digest,request) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, ws, id, req.RequestID, req.Revision, req.WorkflowID, actor, digest, encodeDevelopment(request))
	if err != nil {
		return err
	}
	var storedDigest, storedActor string
	var raw []byte
	if err = s.Pool.QueryRow(ctx, `SELECT request_digest,actor_id,request FROM weave_team_development_trials WHERE workspace_id=$1 AND request_id=$2`, ws, req.RequestID).Scan(&storedDigest, &storedActor, &raw); err != nil {
		return err
	}
	if storedDigest != digest || storedActor != actor {
		return developmentError("此试跑请求已绑定另一份配置或材料")
	}
	if err = json.Unmarshal(raw, &request); err != nil {
		return err
	}
	receipt, err := s.KernelPublication.AdmitCandidate(ctx, request)
	if err != nil {
		return mapWorkflowPublishError(c, err)
	}
	if err = receipt.Verify(ctx, request); err != nil {
		return err
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE weave_team_development_trials SET receipt=$3 WHERE workspace_id=$1 AND request_id=$2`, ws, req.RequestID, encodeDevelopment(receipt)); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, map[string]any{"request_id": req.RequestID, "run_id": receipt.RunID, "revision": req.Revision, "workflow_id": req.WorkflowID, "input_sha256": hex.EncodeToString(hash[:])})
}
func (s *Server) handlePublishTeamDevelopment(c echo.Context) error {
	var req struct {
		Revision int64 `json:"revision"`
	}
	if err := decodeWorkflowBody(c, &req); err != nil {
		return workflowSchemaError(c)
	}
	ctx, ws, id, actor := c.Request().Context(), getTenant(c), c.Param("id"), getUserID(c)
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	d, err := readDevelopment(ctx, tx, ws, id, true)
	if err != nil {
		return err
	}
	if req.Revision != d.Revision {
		return developmentError("草稿已变化，请重新试跑")
	}
	if d.PublishedRevision == d.Revision {
		return s.handleGetTeamDevelopment(c)
	}
	if d.PreparedActor != actor {
		return developmentError("请使用本次试跑的开发者账号发布")
	}
	if d.PreparedRevision != d.Revision || len(d.Prepared) == 0 {
		return developmentError("请先试跑当前草稿")
	}
	for _, p := range d.Prepared {
		var passed bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_team_development_trials t JOIN weave_team_runs r ON r.workspace_id=t.workspace_id AND r.run_id=t.receipt->>'run_id' WHERE t.workspace_id=$1 AND t.team_id=$2 AND t.revision=$3 AND t.workflow_id=$4 AND r.status='succeeded')`, ws, id, d.Revision, p.ID).Scan(&passed)
		if err != nil {
			return err
		}
		if !passed {
			return developmentError("每条待发布流程都需要完成当前草稿的试跑")
		}
	}
	if err = checkDevelopmentBaseline(ctx, tx, ws, id, d.Baseline); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE weave_team_development_drafts SET publishing_revision=$3 WHERE workspace_id=$1 AND team_id=$2`, ws, id, d.Revision); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	for _, p := range d.Prepared {
		request := publication.PublishRequest{Version: publication.ContractVersion, RequestID: "development-publish:" + id + ":" + p.Envelope.ContentHash, Candidate: p.Envelope}
		receipt, err := s.KernelPublication.Publish(ctx, request)
		if err != nil {
			return mapWorkflowPublishError(c, err)
		}
		if err = receipt.Verify(ctx, request); err != nil {
			return err
		}
	}
	if err = s.activateDevelopment(ctx, ws, id, actor, d); err != nil {
		return err
	}
	return s.handleGetTeamDevelopment(c)
}
func (s *Server) handleDevelopmentTrialInput(c echo.Context) error {
	ctx, workspaceID := c.Request().Context(), getTenant(c)
	var raw, receiptRaw []byte
	err := s.Pool.QueryRow(ctx, `SELECT request,COALESCE(receipt,'{}'::jsonb) FROM weave_team_development_trials WHERE workspace_id=$1 AND team_id=$2 AND request_id=$3 AND actor_id=$4`, workspaceID, c.Param("id"), c.Param("request"), getUserID(c)).Scan(&raw, &receiptRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return echo.NewHTTPError(404, "试跑材料不存在或不属于当前账号")
	}
	if err != nil {
		return err
	}
	var req publication.CandidateRunRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		return err
	}
	var receipt publication.AdmissionReceipt
	_ = json.Unmarshal(receiptRaw, &receipt)
	status, output := "submitting", ""
	if receipt.RunID != "" {
		if err = s.Pool.QueryRow(ctx, `SELECT r.status,COALESCE((SELECT q.result->>'output' FROM weave_task_queue q WHERE q.workspace_id=r.workspace_id AND q.run_id=r.run_id AND q.status='completed' AND q.result ? 'output' ORDER BY q.completed_at DESC NULLS LAST,q.id DESC LIMIT 1),'') FROM weave_team_runs r WHERE r.workspace_id=$1 AND r.run_id=$2`, workspaceID, receipt.RunID).Scan(&status, &output); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return c.JSON(200, map[string]any{"input": req.Input, "input_version": req.InputVersion, "workflow_version": req.Candidate.WorkflowVersion, "status": status, "output": output})
}
