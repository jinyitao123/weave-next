package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

type workbenchAuthorization struct {
	Status      string                              `json:"status"`
	Reason      string                              `json:"reason,omitempty"`
	GrantID     string                              `json:"grant_id,omitempty"`
	Generation  int64                               `json:"generation,omitempty"`
	ExpiresAt   *time.Time                          `json:"expires_at,omitempty"`
	Scope       *businessaction.TaskDelegationScope `json:"scope,omitempty"`
	CanRenew    bool                                `json:"can_renew"`
	RetryNodeID string                              `json:"retry_node_id,omitempty"`
}

type inputLineageReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (s *Server) readInputReplacement(ctx context.Context, workspaceID, userID, inputID, rootID string) (string, error) {
	return readInputReplacementFrom(ctx, s.GetPool(), workspaceID, userID, inputID, rootID)
}
func readInputReplacementFrom(ctx context.Context, reader inputLineageReader, workspaceID, userID, inputID, rootID string) (string, error) {
	var replacement string
	err := reader.QueryRow(ctx, `WITH RECURSIVE successors AS (
  SELECT i.input_revision_id,i.consumed_run_id,i.consumed_at,1 AS depth,ARRAY[i.input_revision_id]::text[] AS path
  FROM weave_dispatch_input_revisions i WHERE i.workspace_id=$1 AND i.user_id=$2 AND i.root_input_revision_id=$4 AND i.parent_input_revision_id=$3
  UNION ALL
  SELECT i.input_revision_id,i.consumed_run_id,i.consumed_at,p.depth+1,p.path||i.input_revision_id
  FROM weave_dispatch_input_revisions i JOIN successors p ON i.parent_input_revision_id=p.input_revision_id
  WHERE i.workspace_id=$1 AND i.user_id=$2 AND i.root_input_revision_id=$4 AND NOT(i.input_revision_id=ANY(p.path)) AND p.consumed_run_id IS NOT NULL AND p.consumed_at IS NOT NULL
 ) SELECT COALESCE((SELECT input_revision_id FROM successors WHERE consumed_run_id IS NOT NULL AND consumed_at IS NOT NULL ORDER BY depth DESC,consumed_at DESC,input_revision_id DESC LIMIT 1),'')`, workspaceID, userID, inputID, rootID).Scan(&replacement)
	return replacement, err
}

func (s *Server) readWorkbenchAuthorization(ctx context.Context, workspaceID, userID, inputID string) (workbenchAuthorization, error) {
	result := workbenchAuthorization{Status: "not_applicable"}
	var registration, taskSHA, workflowID, rootID, runID string
	var version int
	var actionsRaw, resourcesRaw, waitRaw []byte
	var generation *int64
	var expires, revoked, closed *time.Time
	var grantID *string
	var runStatus, waitKind *string
	err := s.GetPool().QueryRow(ctx, `SELECT i.registration_id,i.task_sha256,i.workflow_id,i.workflow_version,i.root_input_revision_id,COALESCE(i.consumed_run_id,''),i.closed_at,
 d.allowed_actions,d.resources,d.refresh_generation,d.expires_at,d.revoked_at,d.grant_id,r.status,r.wait_kind,r.wait_detail
 FROM weave_dispatch_input_revisions i LEFT JOIN weave_task_business_delegations d ON d.workspace_id=i.workspace_id AND d.user_id=i.user_id AND d.input_revision_id=i.input_revision_id
 LEFT JOIN weave_team_runs r ON r.workspace_id=i.workspace_id AND r.run_id=i.consumed_run_id
 WHERE i.workspace_id=$1 AND i.user_id=$2 AND i.input_revision_id=$3`, workspaceID, userID, inputID).Scan(&registration, &taskSHA, &workflowID, &version, &rootID, &runID, &closed, &actionsRaw, &resourcesRaw, &generation, &expires, &revoked, &grantID, &runStatus, &waitKind, &waitRaw)
	if err != nil {
		return result, err
	}
	if generation == nil {
		return result, nil
	}
	if grantID != nil {
		result.GrantID = *grantID
	}
	var actions []string
	if json.Unmarshal(actionsRaw, &actions) != nil {
		return result, errors.New("frozen action scope is invalid")
	}
	resources, record, err := projectWorkbenchContextResources(resourcesRaw, inputID, taskSHA)
	if err != nil {
		return result, err
	}
	scope := businessaction.TaskDelegationScope{InputRevisionID: inputID, RegistrationID: registration, TaskSHA256: taskSHA, WorkflowID: workflowID, WorkflowVersion: version, AllowedActions: actions, Resources: taskScopeResources(resources)}
	if record != nil {
		scope.BusinessRecord = &businessaction.TaskBusinessRecord{ObjectName: record.ObjectName, RecordID: record.RecordID}
	}
	result.Scope = &scope
	result.ExpiresAt = expires
	result.Generation = *generation
	result.Status = "active"
	if grantID == nil || *grantID == "" {
		result.Generation = 0
		result.Status = "renewal_required"
		result.Reason = "原授权不具受限任务凭据，需要本人重新授权。"
	}
	if expires == nil || !expires.After(time.Now().UTC()) || revoked != nil {
		result.Status = "renewal_required"
		result.Reason = "本次工作的授权已过期或失效。"
	}
	var detail teamrun.RuntimeWaitDetailV1
	if runStatus != nil && *runStatus == "parked" && waitKind != nil && *waitKind == "runtime" && json.Unmarshal(waitRaw, &detail) == nil && detail.AuthorizationRequired != nil && detail.AuthorizationRequired.Renewable() && !detail.RecoveryBlocked {
		result.RetryNodeID = detail.NodeID
		result.CanRenew = true
		if result.Generation <= detail.AuthorizationRequired.Generation {
			result.Status = "renewal_required"
			result.Reason = "请本人重新授权同一份工作后继续原执行位置。"
		}
	} else if result.Status == "renewal_required" && runStatus != nil && (*runStatus == "failed" || *runStatus == "succeeded") {
		result.Reason = "原运行没有可安全续办的未执行证明，请先核对原结果。"
	}
	replacement, err := s.readInputReplacement(ctx, workspaceID, userID, inputID, rootID)
	if err != nil {
		return result, err
	}
	if closed != nil || replacement != "" {
		result.CanRenew = false
		result.RetryNodeID = ""
		result.Reason = "原输入已关闭或被同一工作的新输入取代。"
	}
	return result, nil
}

func (s *Server) handleRenewDispatchAuthorization(c echo.Context) error {
	workspaceID, userID, inputID := getTenant(c), getUserID(c), strings.TrimSpace(c.Param("input_revision_id"))
	if s.GetPool() == nil {
		return workflowError(c, 503, "business_delegation_unavailable", "Task authorization is unavailable")
	}
	var request struct {
		ExpectedGeneration int64 `json:"expected_generation"`
	}
	if decodeWorkflowBody(c, &request) != nil || request.ExpectedGeneration < 0 {
		return workflowError(c, 400, "business_delegation_generation_conflict", "Authorization generation is invalid")
	}
	state, err := s.readWorkbenchAuthorization(c.Request().Context(), workspaceID, userID, inputID)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowError(c, 404, "dispatch_input_not_found", "Original work was not found")
	}
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if !state.CanRenew || state.Scope == nil {
		return workflowError(c, 409, "authorization_resume_unsafe", "Original work cannot safely resume without a no-effect proof")
	}
	if state.Generation != request.ExpectedGeneration {
		return workflowError(c, 409, "business_delegation_generation_conflict", "Authorization generation changed")
	}
	scope := state.Scope
	resources := make([]dispatchInputResource, 0, len(scope.Resources))
	for _, r := range scope.Resources {
		resources = append(resources, dispatchInputResource{Type: r.Type, SourceKind: r.SourceKind, RequestID: r.RequestID, MaterialID: r.MaterialID, ID: r.ID, Name: r.Name, MediaType: r.MediaType, Bytes: r.Bytes, SHA256: r.SHA256})
	}
	var record *dispatchBusinessRecord
	if scope.BusinessRecord != nil {
		record = &dispatchBusinessRecord{ObjectName: scope.BusinessRecord.ObjectName, RecordID: scope.BusinessRecord.RecordID}
	}
	prepared, rejected := s.prepareBusinessDelegation(c.Request().Context(), workspaceID, userID, c.Request().Header.Get(forgeDelegationHeader), inputID, scope.RegistrationID, scope.TaskSHA256, scope.WorkflowID, scope.WorkflowVersion, scope.AllowedActions, resources, record)
	if rejected != nil {
		status := rejected.status
		if rejected.code == "business_delegation_expired" {
			status = http.StatusConflict
		}
		return workflowError(c, status, rejected.code, rejected.message)
	}
	if prepared == nil || prepared.generation <= state.Generation {
		return workflowError(c, 409, "business_delegation_generation_conflict", "A newer task authorization is required")
	}
	tx, err := s.GetPool().Begin(c.Request().Context())
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	defer tx.Rollback(c.Request().Context())
	var generation int64
	var oldGrant string
	if err := tx.QueryRow(c.Request().Context(), `SELECT refresh_generation,grant_id FROM weave_task_business_delegations WHERE workspace_id=$1 AND user_id=$2 AND input_revision_id=$3 FOR UPDATE`, workspaceID, userID, inputID).Scan(&generation, &oldGrant); err != nil {
		return workflowStoreFailure(c, err)
	}
	var rootID, runStatus, waitKind string
	var closed *time.Time
	var waitRaw []byte
	if err := tx.QueryRow(c.Request().Context(), `SELECT i.root_input_revision_id,i.closed_at,r.status,COALESCE(r.wait_kind,''),r.wait_detail FROM weave_dispatch_input_revisions i JOIN weave_team_runs r ON r.workspace_id=i.workspace_id AND r.run_id=i.consumed_run_id WHERE i.workspace_id=$1 AND i.user_id=$2 AND i.input_revision_id=$3 FOR SHARE OF i,r`, workspaceID, userID, inputID).Scan(&rootID, &closed, &runStatus, &waitKind, &waitRaw); err != nil {
		return workflowStoreFailure(c, err)
	}
	var liveWait teamrun.RuntimeWaitDetailV1
	if closed != nil || runStatus != "parked" || waitKind != "runtime" || json.Unmarshal(waitRaw, &liveWait) != nil || liveWait.RecoveryBlocked || liveWait.AuthorizationRequired == nil || !liveWait.AuthorizationRequired.Renewable() || liveWait.AuthorizationRequired.InputRevisionID != inputID {
		return workflowError(c, 409, "authorization_resume_unsafe", "Original work is no longer waiting on a safe authorization refusal")
	}
	if replacement, err := readInputReplacementFrom(c.Request().Context(), tx, workspaceID, userID, inputID, rootID); err != nil {
		return workflowStoreFailure(c, err)
	} else if replacement != "" {
		return workflowError(c, 409, "authorization_resume_unsafe", "Original input was superseded")
	}
	if oldGrant == "" {
		generation = 0
	}
	if generation != request.ExpectedGeneration {
		return workflowError(c, 409, "business_delegation_generation_conflict", "Authorization generation changed")
	}
	if err := persistBusinessDelegationTx(c.Request().Context(), tx, prepared, workspaceID, userID, inputID, scope.TaskSHA256, scope.WorkflowID, scope.WorkflowVersion); err != nil {
		return workflowStoreFailure(c, err)
	}
	if err := tx.Commit(c.Request().Context()); err != nil {
		return workflowStoreFailure(c, err)
	}
	return c.JSON(200, map[string]any{"input_revision_id": inputID, "generation": prepared.generation, "expires_at": prepared.expiresAt, "status": "active"})
}
