package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
	"github.com/labstack/echo/v4"
)

// A dedicated connection owns this short distributed lock; holding pooled
// connections while the shared dispatcher starts its transaction would starve
// a small pool when many lanes admit concurrently. Cancellation uses this
// exact lane lock too, including cancellation before an admission row exists.
func (s *Server) lockDecisionLane(ctx context.Context, workspace, key, lane string) (func(), error) {
	conn, err := pgx.ConnectConfig(ctx, s.GetPool().Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "decision\x1f"+workspace+"\x1f"+key+"\x1f"+lane)
	if err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}, nil
}

func decisionServiceKey(c echo.Context) string {
	if c.Get(authSourceContextKey) != authSourceAPIKey {
		return ""
	}
	id, _ := c.Get(apiKeyIDContextKey).(string)
	return id
}

func (s *Server) decisionArtifact(ctx context.Context, workspace, workflowID string, version int) (frozen.ArtifactPayloadV1, error) {
	artifact, err := s.WorkflowArtifacts.GetArtifact(ctx, workspace, workflowID, version)
	if err != nil {
		return frozen.ArtifactPayloadV1{}, err
	}
	return frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{WorkspaceID: artifact.WorkspaceID, WorkflowID: artifact.WorkflowID, WorkflowVersion: artifact.WorkflowVersion, ArtifactSchemaVersion: artifact.ArtifactSchemaVersion, CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm, CanonicalizationVersion: artifact.CanonicalizationVersion, HashAlgorithm: artifact.HashAlgorithm, ContentHash: artifact.ContentHash, Payload: artifact.Payload})
}

func (s *Server) handleBindDecision(c echo.Context) error {
	if s.GetPool() == nil || s.Workflow == nil || s.WorkflowArtifacts == nil {
		return workflowError(c, 503, "decision_unavailable", "decision service unavailable")
	}
	var b decisionBinding
	if err := decodeWorkflowBody(c, &b); err != nil || b.TeamID == "" || b.WorkflowID == "" || b.WorkflowVersion < 1 {
		return workflowError(c, 400, "invalid_decision_binding", "an exact team, published workflow version and contract are required")
	}
	if err := b.Contract.normalize(); err != nil {
		return workflowError(c, 400, "invalid_decision_contract", err.Error())
	}
	payload, err := s.decisionArtifact(c.Request().Context(), getTenant(c), b.WorkflowID, b.WorkflowVersion)
	var belongs bool
	if err == nil {
		err = s.GetPool().QueryRow(c.Request().Context(), `SELECT EXISTS(SELECT 1 FROM weave_team_workflows WHERE workspace_id=$1 AND id=$2 AND team_id=$3)`, getTenant(c), b.WorkflowID, b.TeamID).Scan(&belongs)
	}
	if err != nil || !belongs {
		return workflowError(c, 409, "decision_workflow_unpublished", "workflow must belong to this team and be published")
	}
	if _, err := validateDecisionWorkflow(payload); err != nil {
		return workflowError(c, 409, "unsafe_decision_workflow", err.Error())
	}
	contract, _ := json.Marshal(b.Contract)
	tag, err := s.GetPool().Exec(c.Request().Context(), `INSERT INTO weave_decision_bindings(workspace_id,api_key_id,team_id,workflow_id,workflow_version,contract,contract_hash,created_by) SELECT $1,id,$3,$4,$5,$6,$7,$8 FROM weave_api_keys WHERE id=$2 AND tenant_id=$1 AND scopes=ARRAY['decisions']::text[] ON CONFLICT (workspace_id,api_key_id) DO UPDATE SET team_id=EXCLUDED.team_id,workflow_id=EXCLUDED.workflow_id,workflow_version=EXCLUDED.workflow_version,contract=EXCLUDED.contract,contract_hash=EXCLUDED.contract_hash,updated_at=NOW()`, getTenant(c), c.Param("key_id"), b.TeamID, b.WorkflowID, b.WorkflowVersion, string(contract), b.Contract.hash(), getUserID(c))
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if tag.RowsAffected() == 0 {
		return workflowError(c, 409, "decision_service_key_required", "key must belong to this workspace and have only the decisions scope")
	}
	return c.JSON(200, map[string]any{"team_id": b.TeamID, "workflow_id": b.WorkflowID, "workflow_version": b.WorkflowVersion, "contract": b.Contract, "contract_hash": b.Contract.hash()})
}

func (s *Server) handleAdmitDecision(c echo.Context) error {
	key := decisionServiceKey(c)
	if key == "" {
		return workflowError(c, 403, "decision_service_key_required", "a bound service API key is required")
	}
	if s.GetPool() == nil || s.Workflow == nil || s.WorkflowArtifacts == nil || s.Registry == nil || s.OrgStore == nil {
		return workflowError(c, 503, "decision_unavailable", "decision service unavailable")
	}
	var req struct {
		ClientRequestID string          `json:"client_request_id"`
		Lane            string          `json:"lane"`
		Input           json.RawMessage `json:"input"`
	}
	if err := decodeWorkflowBody(c, &req); err != nil {
		return workflowError(c, 400, "invalid_decision_input", "invalid decision request")
	}
	if _, err := uuid.Parse(req.ClientRequestID); err != nil {
		return workflowError(c, 400, "invalid_decision_request_id", "client_request_id must be UUID")
	}
	if !decisionLane.MatchString(req.Lane) {
		return workflowError(c, 400, "invalid_decision_lane", "lane must be 1-64 letters, digits, '_', '.', ':' or '-'")
	}
	unlock, err := s.lockDecisionLane(c.Request().Context(), getTenant(c), key, req.Lane)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	defer unlock()
	var b decisionBinding
	var contract []byte
	err = s.GetPool().QueryRow(c.Request().Context(), `SELECT team_id,workflow_id,workflow_version,contract FROM weave_decision_bindings WHERE workspace_id=$1 AND api_key_id=$2`, getTenant(c), key).Scan(&b.TeamID, &b.WorkflowID, &b.WorkflowVersion, &contract)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowError(c, 403, "decision_service_unbound", "service key has no workflow binding")
	}
	if err != nil || json.Unmarshal(contract, &b.Contract) != nil {
		return workflowStoreFailure(c, fmt.Errorf("read decision binding: %v", err))
	}
	canonical, err := b.Contract.validateInput(req.Input)
	if err != nil {
		return workflowError(c, 400, "invalid_decision_input", err.Error())
	}
	h := sha256.Sum256(canonical)
	inputHash := fmt.Sprintf("%x", h[:])
	var cancelled bool
	err = s.GetPool().QueryRow(c.Request().Context(), `SELECT EXISTS(SELECT 1 FROM weave_decision_cancellations WHERE workspace_id=$1 AND api_key_id=$2 AND client_request_id=$3)`, getTenant(c), key, req.ClientRequestID).Scan(&cancelled)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if cancelled {
		return workflowError(c, 409, "decision_cancelled", "this exact request was cancelled")
	}
	var activeRequest string
	err = s.GetPool().QueryRow(c.Request().Context(), `SELECT a.client_request_id::text FROM weave_decision_admissions a LEFT JOIN weave_team_runs r ON r.workspace_id=a.workspace_id AND r.run_id=a.run_id LEFT JOIN weave_decision_cancellations x ON x.workspace_id=a.workspace_id AND x.api_key_id=a.api_key_id AND x.client_request_id=a.client_request_id WHERE a.workspace_id=$1 AND a.api_key_id=$2 AND a.lane=$3 AND a.client_request_id<>$4 AND ((r.run_id IS NULL AND x.client_request_id IS NULL) OR r.status NOT IN ('succeeded','failed','cancelled','abandoned')) ORDER BY a.created_at LIMIT 1`, getTenant(c), key, req.Lane, req.ClientRequestID).Scan(&activeRequest)
	if err == nil {
		c.Response().Header().Set("X-Decision-Active-Request-ID", activeRequest)
		return workflowError(c, 409, "decision_lane_inflight", "the previous decision in this lane must terminate or be cancelled first")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return workflowStoreFailure(c, err)
	}
	decisionID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(getTenant(c)+"\x1f"+key+"\x1f"+req.ClientRequestID)).String()
	// A dedicated request namespace prevents the owner or another service key
	// colliding with this receipt in generic dispatch idempotency.
	dispatchRequestID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("bounded-decision\x1f"+key+"\x1f"+req.ClientRequestID)).String()
	runID, taskID := deterministicWorkflowDispatchIDs(getTenant(c), getUserID(c), dispatchRequestID)
	_, err = s.GetPool().Exec(c.Request().Context(), `INSERT INTO weave_decision_admissions(workspace_id,api_key_id,decision_id,client_request_id,lane,input_hash,input_json,contract,run_id,task_id,team_id,workflow_id,workflow_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT DO NOTHING`, getTenant(c), key, decisionID, req.ClientRequestID, req.Lane, inputHash, string(canonical), string(contract), runID, taskID, b.TeamID, b.WorkflowID, b.WorkflowVersion)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	var storedHash, storedLane string
	var storedContract []byte
	err = s.GetPool().QueryRow(c.Request().Context(), `SELECT input_hash,lane,contract,team_id,workflow_id,workflow_version FROM weave_decision_admissions WHERE workspace_id=$1 AND api_key_id=$2 AND decision_id=$3`, getTenant(c), key, decisionID).Scan(&storedHash, &storedLane, &storedContract, &b.TeamID, &b.WorkflowID, &b.WorkflowVersion)
	if err != nil || json.Unmarshal(storedContract, &b.Contract) != nil {
		return workflowStoreFailure(c, fmt.Errorf("read decision admission: %v", err))
	}
	if storedHash != inputHash || storedLane != req.Lane {
		return workflowError(c, 409, "decision_input_conflict", "same request id has a different frozen input or lane")
	}
	payload, err := s.decisionArtifact(c.Request().Context(), getTenant(c), b.WorkflowID, b.WorkflowVersion)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if _, err := validateDecisionWorkflow(payload); err != nil {
		return workflowError(c, 409, "unsafe_decision_workflow", err.Error())
	}
	c.Response().Header().Set("X-Decision-ID", decisionID)
	c.Response().Header().Set("X-Decision-Input-Hash", inputHash)
	c.SetParamNames("id")
	c.SetParamValues(b.TeamID)
	return s.dispatchAdmittedTeam(c, teamDispatchRequest{Task: b.Contract.task(canonical), Mode: teamDispatchModeWorkflow, WorkflowID: b.WorkflowID, WorkflowVersion: &b.WorkflowVersion, ClientRequestID: dispatchRequestID})
}

func (s *Server) handleCancelDecision(c echo.Context) error {
	key := decisionServiceKey(c)
	if key == "" {
		return workflowError(c, 403, "decision_service_key_required", "service API key required")
	}
	if s.GetPool() == nil || s.teamRunCancel == nil {
		return workflowError(c, 503, "decision_unavailable", "decision service unavailable")
	}
	var req struct {
		ClientRequestID string `json:"client_request_id"`
		Lane            string `json:"lane"`
	}
	if err := decodeWorkflowBody(c, &req); err != nil || !decisionLane.MatchString(req.Lane) {
		return workflowError(c, 400, "invalid_decision_cancel", "lane and request identity required")
	}
	if _, err := uuid.Parse(req.ClientRequestID); err != nil {
		return workflowError(c, 400, "invalid_decision_cancel", "request identity must be UUID")
	}
	unlock, err := s.lockDecisionLane(c.Request().Context(), getTenant(c), key, req.Lane)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	defer unlock()
	var runID, lane string
	err = s.GetPool().QueryRow(c.Request().Context(), `SELECT run_id,lane FROM weave_decision_admissions WHERE workspace_id=$1 AND api_key_id=$2 AND client_request_id=$3`, getTenant(c), key, req.ClientRequestID).Scan(&runID, &lane)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return workflowStoreFailure(c, err)
	}
	if lane != "" && lane != req.Lane {
		return workflowError(c, 409, "decision_cancel_lane_mismatch", "request belongs to another lane")
	}
	_, err = s.GetPool().Exec(c.Request().Context(), `INSERT INTO weave_decision_cancellations(workspace_id,api_key_id,client_request_id,lane) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, getTenant(c), key, req.ClientRequestID, req.Lane)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if runID == "" {
		return c.JSON(200, map[string]any{"status": "cancelled", "client_request_id": req.ClientRequestID})
	}
	run, err := s.teamRunCancel.RequestCancel(c.Request().Context(), teamrun.CancelRequest{WorkspaceID: getTenant(c), RunID: runID, CancelActor: getUserID(c), CancelReason: "decision_expired", IdempotencyKey: "decision-cancel:" + req.ClientRequestID, GraceDeadline: time.Now().UTC().Add(5 * time.Second)})
	if errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
		return c.JSON(200, map[string]any{"status": "cancelled", "client_request_id": req.ClientRequestID})
	}
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(200, map[string]any{"status": run.Status, "run_id": runID, "client_request_id": req.ClientRequestID})
}

func (s *Server) handleReadDecision(c echo.Context) error {
	key := decisionServiceKey(c)
	if key == "" {
		return workflowError(c, 403, "decision_service_key_required", "service API key required")
	}
	if s.GetPool() == nil || s.Workflow == nil || s.WorkflowArtifacts == nil || s.teamRunCancel == nil || s.teamRunCancel.Runs == nil || s.Deliverables == nil {
		return workflowError(c, 503, "decision_unavailable", "decision service unavailable")
	}
	ctx := c.Request().Context()
	var runID, inputHash, lane string
	var input, contract []byte
	var b decisionBinding
	err := s.GetPool().QueryRow(ctx, `SELECT run_id,input_hash,lane,input_json,contract,team_id,workflow_id,workflow_version FROM weave_decision_admissions WHERE workspace_id=$1 AND api_key_id=$2 AND decision_id=$3`, getTenant(c), key, c.Param("decision_id")).Scan(&runID, &inputHash, &lane, &input, &contract, &b.TeamID, &b.WorkflowID, &b.WorkflowVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowError(c, 404, "decision_not_found", "decision not found")
	}
	if err != nil || json.Unmarshal(contract, &b.Contract) != nil {
		return workflowStoreFailure(c, fmt.Errorf("read decision admission: %v", err))
	}
	run, err := s.teamRunCancel.Runs.Get(ctx, getTenant(c), runID)
	if err != nil {
		if !errors.Is(err, teamrun.ErrTeamRunIdentityMismatch) {
			return workflowStoreFailure(c, err)
		}
		var cancelled bool
		if err := s.GetPool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_decision_cancellations x JOIN weave_decision_admissions a USING(workspace_id,api_key_id,client_request_id) WHERE a.workspace_id=$1 AND a.api_key_id=$2 AND a.decision_id=$3)`, getTenant(c), key, c.Param("decision_id")).Scan(&cancelled); err != nil {
			return workflowStoreFailure(c, err)
		}
		status := "admitting"
		if cancelled {
			status = "cancelled"
		}
		return c.JSON(202, map[string]any{"decision_id": c.Param("decision_id"), "run_id": runID, "input_hash": inputHash, "lane": lane, "status": status})
	}
	payload, err := s.decisionArtifact(ctx, getTenant(c), b.WorkflowID, b.WorkflowVersion)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		return workflowError(c, 409, "decision_workflow_invalid", "frozen decision workflow cannot be decoded")
	}
	items, err := s.Deliverables.List(ctx, getTenant(c), deliverable.ListFilter{RunID: runID, Limit: 101})
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	var events []teamrun.ActivityEvent
	if s.teamRunActivities != nil {
		events, _ = s.teamRunActivities.List(ctx, getTenant(c), runID, 501)
	}
	result := map[string]any{"decision_id": c.Param("decision_id"), "run_id": runID, "run_snapshot_id": run.RunSnapshotID, "input_hash": inputHash, "lane": lane, "status": run.Status, "workflow_id": b.WorkflowID, "workflow_version": b.WorkflowVersion, "delivery": s.runDelivery(ctx, run), "trace": decisionTrace(payload, graph, run, items, events)}
	if run.Status != teamrun.StatusSucceeded {
		return c.JSON(200, result)
	}
	var found json.RawMessage
	for _, item := range items {
		var meta map[string]any
		_ = json.Unmarshal(item.Metadata, &meta)
		if meta["artifact_kind"] != "final" || item.RunSnapshotID != run.RunSnapshotID {
			continue
		}
		if found != nil {
			return workflowError(c, 409, "ambiguous_decision_output", "workflow returned more than one final output")
		}
		found = json.RawMessage(item.Content)
	}
	if err := b.Contract.validateChoice(found, input); err != nil {
		return workflowError(c, 422, "invalid_decision_output", err.Error())
	}
	decider, nodeID := decisionDecider(payload, graph)
	if decider == nil || decider.Agent.Model == "" {
		return workflowError(c, 409, "decision_provenance_missing", "frozen deciding member identity missing")
	}
	result["choice"] = found
	result["decider_node_id"] = nodeID
	result["agent_id"] = decider.Agent.AgentID
	result["agent_version"] = decider.Agent.AgentVersion
	result["model"] = decider.Agent.Model
	result["engine"] = decider.Agent.Engine
	result["runtime_id"] = decider.Agent.RuntimeID
	return c.JSON(http.StatusOK, result)
}
