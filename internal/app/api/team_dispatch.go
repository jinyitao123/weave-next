package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/kernel/org"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

const (
	teamDispatchModeWorkflow   = "workflow"
	teamDispatchModeFreeCollab = "free_collab"
)

type teamDispatchRequest struct {
	Task            string `json:"task"`
	Mode            string `json:"mode,omitempty"`
	WorkflowID      string `json:"workflow_id,omitempty"`
	WorkflowVersion *int   `json:"workflow_version,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	ProjectID       string `json:"project_id,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
}

// handleDispatchTeam is the single product dispatch boundary. It freezes the
// requested execution mode before enqueueing and never falls back from a fixed
// workflow to free collaboration.
func (s *Server) handleDispatchTeam(c echo.Context) error {
	if s.OrgStore == nil || s.Registry == nil || s.Workflow == nil {
		return workflowError(c, http.StatusServiceUnavailable, "team_dispatch_unavailable", "team dispatch service unavailable")
	}
	var request teamDispatchRequest
	if err := decodeWorkflowBody(c, &request); err != nil {
		return workflowError(c, http.StatusBadRequest, "team_dispatch_request_invalid", "team dispatch request invalid")
	}
	request.Task = strings.TrimSpace(request.Task)
	request.Mode = strings.TrimSpace(request.Mode)
	request.WorkflowID = strings.TrimSpace(request.WorkflowID)
	request.ClientRequestID = strings.TrimSpace(request.ClientRequestID)
	request.ProjectID = strings.TrimSpace(request.ProjectID)
	request.ConversationID = strings.TrimSpace(request.ConversationID)
	if request.Task == "" || (request.WorkflowVersion != nil && *request.WorkflowVersion <= 0) {
		return workflowError(c, http.StatusBadRequest, "team_dispatch_request_invalid", "team dispatch request invalid")
	}
	if request.Mode == "" {
		request.Mode = teamDispatchModeWorkflow
	}
	if request.Mode != teamDispatchModeWorkflow && request.Mode != teamDispatchModeFreeCollab {
		return workflowError(c, http.StatusBadRequest, "team_dispatch_mode_invalid", "team dispatch mode must be workflow or free_collab")
	}
	if request.ClientRequestID == "" {
		request.ClientRequestID = uuid.NewString()
	} else if _, err := uuid.Parse(request.ClientRequestID); err != nil {
		return workflowError(c, http.StatusBadRequest, "invalid_client_request_id", "client_request_id must be a UUID")
	}

	team, err := s.OrgStore.GetTeam(c.Request().Context(), getTenant(c), c.Param("id"))
	if errors.Is(err, org.ErrTeamNotFound) {
		return workflowError(c, http.StatusNotFound, "team_not_found", "team not found")
	}
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if team.Status != "active" {
		return workflowError(c, http.StatusConflict, "team_not_active", "team is not active")
	}

	if request.Mode == teamDispatchModeFreeCollab {
		if request.WorkflowID != "" || request.WorkflowVersion != nil {
			return workflowError(c, http.StatusBadRequest, "team_dispatch_request_invalid", "free_collab forbids workflow selection")
		}
		return s.dispatchTeamFreeCollab(c, team, request)
	}
	runID, taskID := deterministicWorkflowDispatchIDs(getTenant(c), getUserID(c), request.ClientRequestID)
	fingerprint := workflowDispatchFingerprint(team.ID, request)
	if replayed, err := s.replayWorkflowDispatch(c, team, request, runID, taskID, fingerprint); replayed || err != nil {
		return err
	}
	c.Set("workflow_dispatch_run_id", runID)
	c.Set("workflow_dispatch_task_id", taskID)
	c.Set("workflow_dispatch_fingerprint", fingerprint)
	return s.dispatchTeamWorkflow(c, team, request)
}

func workflowDispatchFingerprint(teamID string, request teamDispatchRequest) string {
	encoded, _ := json.Marshal(struct {
		TeamID          string `json:"team_id"`
		Task            string `json:"task"`
		Mode            string `json:"mode"`
		WorkflowID      string `json:"workflow_id,omitempty"`
		WorkflowVersion *int   `json:"workflow_version,omitempty"`
		ProjectID       string `json:"project_id,omitempty"`
		ConversationID  string `json:"conversation_id,omitempty"`
	}{teamID, request.Task, request.Mode, request.WorkflowID, request.WorkflowVersion, request.ProjectID, request.ConversationID})
	digest := sha256.Sum256(encoded)
	return "team-dispatch:" + request.ClientRequestID + ":" + fmt.Sprintf("%x", digest[:])
}

func deterministicWorkflowDispatchIDs(workspaceID, userID, clientRequestID string) (string, string) {
	identity := strings.Join([]string{workspaceID, userID, clientRequestID}, "\x1f")
	return "run-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("team-dispatch-run\x1f"+identity)).String(),
		"task-" + uuid.NewSHA1(uuid.NameSpaceOID, []byte("team-dispatch-task\x1f"+identity)).String()
}

func (s *Server) replayWorkflowDispatch(
	c echo.Context,
	team org.Team,
	request teamDispatchRequest,
	runID, taskID, fingerprint string,
) (bool, error) {
	existing, existingTeamID, payload, contextKey, found, err := s.loadWorkflowDispatchReplay(
		c.Request().Context(), getTenant(c), runID, taskID,
	)
	if err != nil {
		return true, workflowStoreFailure(c, err)
	}
	if !found {
		return false, nil
	}
	var originalTask string
	if contextKey != fingerprint || json.Unmarshal([]byte(payload), &originalTask) != nil || existingTeamID != team.ID ||
		originalTask != request.Task || existing.ProjectID != request.ProjectID ||
		(request.WorkflowID != "" && request.WorkflowID != existing.WorkflowID) ||
		(request.WorkflowVersion != nil && *request.WorkflowVersion != existing.WorkflowVersion) {
		return true, workflowError(c, http.StatusConflict, "client_request_conflict", "client_request_id was already used for different dispatch facts")
	}
	existing.RunID = runID
	existing.ConversationID = request.ConversationID
	return true, c.JSON(http.StatusOK, existing)
}

func (s *Server) loadWorkflowDispatchReplay(
	ctx context.Context,
	workspaceID, runID, taskID string,
) (workflowManualRunResponse, string, string, string, bool, error) {
	if s.GetPool() == nil {
		return workflowManualRunResponse{}, "", "", "", false, nil
	}
	var existing workflowManualRunResponse
	var teamID, payload, contextKey string
	err := s.GetPool().QueryRow(ctx, `
		SELECT snapshot.team_id, snapshot.workflow_id, snapshot.workflow_version,
			task.id, COALESCE(snapshot.project_id,''), task.payload::text, COALESCE(task.context_key,'')
		FROM weave_team_run_snapshots AS snapshot
		JOIN weave_task_queue AS task
		  ON task.workspace_id=snapshot.workspace_id
		 AND task.run_snapshot_id=snapshot.run_id
		 AND task.id=$3
		WHERE snapshot.workspace_id=$1 AND snapshot.run_id=$2
		  AND snapshot.mode='fixed_workflow'
	`, workspaceID, runID, taskID).Scan(
		&teamID, &existing.WorkflowID, &existing.WorkflowVersion,
		&existing.TaskID, &existing.ProjectID, &payload, &contextKey,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflowManualRunResponse{}, "", "", "", false, nil
	}
	if err != nil {
		return workflowManualRunResponse{}, "", "", "", false, err
	}
	return existing, teamID, payload, contextKey, true, nil
}

func (s *Server) dispatchTeamFreeCollab(c echo.Context, team org.Team, request teamDispatchRequest) error {
	agents, err := s.Registry.List(c.Request().Context(), getTenant(c))
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	leadName := ""
	for _, agent := range agents {
		if agent.ID == team.LeadAvatarID && !agent.Deleted {
			leadName = agent.Name
			break
		}
	}
	if leadName == "" {
		return workflowError(c, http.StatusConflict, "team_lead_unavailable", "team lead is unavailable")
	}
	return s.handleChatRequest(c, ChatRequest{
		Agent: leadName, ProjectID: request.ProjectID, ConversationID: request.ConversationID,
		ClientRequestID: request.ClientRequestID, Message: request.Task,
		Async: true, Stream: false,
	})
}

func (s *Server) dispatchTeamWorkflow(c echo.Context, team org.Team, request teamDispatchRequest) error {
	workflowID := request.WorkflowID
	if workflowID == "" {
		workflowID = strings.TrimSpace(team.DefaultWorkflowID)
		if workflowID == "" {
			return workflowError(c, http.StatusConflict, "no_default_workflow", "team has no default workflow; select a workflow or use explicit free_collab mode")
		}
	}
	selected, err := s.Workflow.Get(c.Request().Context(), getTenant(c), workflowID)
	if errors.Is(err, workflow.ErrNotFound) {
		code := "workflow_not_found"
		if request.WorkflowID == "" {
			code = "default_workflow_unavailable"
		}
		return workflowError(c, http.StatusConflict, code, "selected workflow is unavailable")
	}
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if selected.TeamID != team.ID {
		return workflowError(c, http.StatusConflict, "workflow_team_mismatch", "selected workflow does not belong to the team")
	}

	input, _ := json.Marshal(request.Task)
	body := map[string]any{"input": json.RawMessage(input)}
	if request.WorkflowVersion != nil {
		body["workflow_version"] = *request.WorkflowVersion
	}
	if request.ProjectID != "" {
		body["project_id"] = request.ProjectID
	}
	if request.ConversationID != "" {
		body["conversation_id"] = request.ConversationID
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	c.SetPath(legacyWorkflowManualRunPath)
	c.SetParamNames("id")
	c.SetParamValues(workflowID)
	requestBody := io.NopCloser(bytes.NewReader(encoded))
	c.SetRequest(c.Request().Clone(c.Request().Context()))
	c.Request().Body = requestBody
	c.Request().ContentLength = int64(len(encoded))
	return s.handleRunWorkflow(c)
}
