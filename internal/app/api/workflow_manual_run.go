package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/projects"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

type workflowManualRunResponse struct {
	RunID           string `json:"run_id"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
	TaskID          string `json:"task_id"`
	ProjectID       string `json:"project_id,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
}

// admitTeamWorkflowDispatch freezes and enqueues the team dispatch already validated by the product boundary.
func (s *Server) admitTeamWorkflowDispatch(c echo.Context, workflowID string, request teamDispatchRequest) error {
	if s.Workflow == nil || s.ScheduleTransactions == nil ||
		s.Snapshots == nil || s.Tasks == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_run_unavailable",
			"workflow run service unavailable",
		)
	}
	projectID, conversationID := request.ProjectID, request.ConversationID
	payload, err := json.Marshal(request.Task)
	if err != nil {
		return workflowSchemaError(c)
	}
	workspaceID := getTenant(c)
	ctx := c.Request().Context()
	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return workflowStoreFailure(c, fmt.Errorf("begin manual workflow run: %w", err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sourceRef := getUserID(c)
	if sourceRef == "" {
		sourceRef = "manual"
	}
	triggerType := ""
	expectedTrigger := "manual"
	taskSource := "manual"
	if conversationID != "" {
		sourceRef = conversationID
		triggerType = "conversation_explicit"
		expectedTrigger = triggerType
		taskSource = "session"
	}
	admitted, err := s.Workflow.AdmitWorkflowManualRunTx(
		ctx,
		tx,
		workflow.WorkflowManualRunAdmissionRequest{
			WorkspaceID:     workspaceID,
			WorkflowID:      workflowID,
			WorkflowVersion: request.WorkflowVersion,
			SourceRef:       sourceRef,
			TriggerType:     triggerType,
		},
	)
	if err != nil {
		return s.respondWorkflowManualRunAdmissionError(c, tx, workflowID, err, expectedTrigger)
	}
	if dispatchRunID, ok := c.Get("workflow_dispatch_run_id").(string); ok && dispatchRunID != "" {
		admitted.RunID = dispatchRunID
	}
	if err := validateWorkflowManualRunSnapshot(
		admitted, workspaceID, workflowID, sourceRef, expectedTrigger,
	); err != nil {
		return workflowStoreFailure(c, err)
	}
	var project projects.Project
	conversationAgentID := ""
	if conversationID != "" {
		var conversationProjectID string
		if err := tx.QueryRow(ctx, `
			SELECT project_id, agent_id
			FROM weave_conversations
			WHERE workspace_id=$1 AND id=$2 AND parent_message_id IS NULL
			FOR SHARE
		`, workspaceID, conversationID).Scan(&conversationProjectID, &conversationAgentID); errors.Is(err, pgx.ErrNoRows) {
			return workflowError(c, http.StatusNotFound, "workflow_run_conversation_not_found", "conversation not found in current workspace")
		} else if err != nil {
			return workflowStoreFailure(c, fmt.Errorf("read workflow run conversation: %w", err))
		} else if projectID != "" && conversationProjectID != projectID {
			return workflowError(c, http.StatusConflict, "workflow_run_conversation_project_mismatch", "conversation does not belong to selected project")
		} else if projectID == "" {
			projectID = conversationProjectID
		}
	}
	if projectID != "" && s.Projects == nil {
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_run_project_unavailable",
			"project store unavailable for attributed workflow run",
		)
	}
	if projectID != "" {
		project, err = loadWorkflowManualRunProject(ctx, tx, workspaceID, projectID)
		if err != nil {
			return workflowManualRunProjectError(c, err)
		}
	}
	if projectID != "" {
		leadAvatarID, _, err := s.workflowManualRunLead(ctx, admitted)
		if err != nil {
			return workflowStoreFailure(c, err)
		}
		if project.AvatarID != leadAvatarID {
			return workflowError(
				c,
				http.StatusConflict,
				"workflow_run_project_avatar_mismatch",
				"project avatar does not match admitted workflow lead avatar",
			)
		}
		if conversationAgentID != "" && conversationAgentID != leadAvatarID {
			return workflowError(
				c,
				http.StatusConflict,
				"workflow_run_conversation_avatar_mismatch",
				"conversation avatar does not match admitted workflow lead avatar",
			)
		}
		admitted.ProjectID = project.ID
	}

	createdSnapshot, err := s.Snapshots.CreateTx(ctx, tx, admitted)
	if err != nil {
		if errors.Is(err, snapshot.ErrAlreadyExists) {
			if fingerprint, ok := c.Get("workflow_dispatch_fingerprint").(string); ok && fingerprint != "" {
				_ = tx.Rollback(ctx)
				taskID, _ := c.Get("workflow_dispatch_task_id").(string)
				existing, _, existingPayload, contextKey, found, replayErr := s.loadWorkflowDispatchReplay(
					ctx, workspaceID, admitted.RunID, taskID,
				)
				if replayErr != nil {
					return workflowStoreFailure(c, replayErr)
				}
				if found && contextKey == fingerprint && existingPayload == string(payload) {
					existing.RunID = admitted.RunID
					existing.ConversationID = conversationID
					return c.JSON(http.StatusOK, existing)
				}
				return workflowError(c, http.StatusConflict, "client_request_conflict", "client_request_id was already used for different dispatch facts")
			}
		}
		return workflowStoreFailure(c, fmt.Errorf("create manual workflow snapshot: %w", err))
	}
	taskID := "task-" + uuid.NewString()
	if dispatchTaskID, ok := c.Get("workflow_dispatch_task_id").(string); ok && dispatchTaskID != "" {
		taskID = dispatchTaskID
	}
	task := &taskqueue.Task{
		ID:                    taskID,
		WorkspaceID:           createdSnapshot.WorkspaceID,
		ProjectID:             createdSnapshot.ProjectID,
		IdentityKind:          taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2,
		WorkflowID:            createdSnapshot.WorkflowID,
		WorkflowVersion:       createdSnapshot.WorkflowVersion,
		RunSnapshotID:         createdSnapshot.RunID,
		Source:                taskSource,
		Kind:                  "team_workflow",
		Payload:               payload,
	}
	if dispatchFingerprint, ok := c.Get("workflow_dispatch_fingerprint").(string); ok {
		task.ContextKey = dispatchFingerprint
	}
	if err := s.Tasks.EnqueueTx(ctx, tx, task); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("enqueue manual workflow task: %w", err))
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("commit manual workflow run: %w", err))
	}

	return c.JSON(http.StatusCreated, workflowManualRunResponse{
		RunID:           createdSnapshot.RunID,
		WorkflowID:      createdSnapshot.WorkflowID,
		WorkflowVersion: createdSnapshot.WorkflowVersion,
		TaskID:          task.ID,
		ProjectID:       createdSnapshot.ProjectID,
		ConversationID:  conversationID,
	})
}

func loadWorkflowManualRunProject(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, projectID string,
) (projects.Project, error) {
	var project projects.Project
	var archived bool
	err := tx.QueryRow(ctx, `
		SELECT id, workspace_id, avatar_id, archived_at IS NOT NULL
		FROM weave_projects
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, projectID).Scan(
		&project.ID, &project.WorkspaceID, &project.AvatarID, &archived,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return projects.Project{}, projects.ErrNotFound
	}
	if err != nil {
		return projects.Project{}, &workflowManualRunProjectUnavailableError{cause: err}
	}
	if archived {
		return projects.Project{}, projects.ErrArchived
	}
	return project, nil
}

type workflowManualRunProjectUnavailableError struct {
	cause error
}

func (e *workflowManualRunProjectUnavailableError) Error() string {
	return "manual workflow project is unavailable"
}

func (e *workflowManualRunProjectUnavailableError) Unwrap() error {
	return e.cause
}

func workflowManualRunProjectError(c echo.Context, err error) error {
	var unavailable *workflowManualRunProjectUnavailableError
	switch {
	case errors.Is(err, projects.ErrNotFound):
		return workflowError(
			c,
			http.StatusNotFound,
			"workflow_run_project_not_found",
			"project not found in current workspace",
		)
	case errors.Is(err, projects.ErrArchived):
		return workflowError(
			c,
			http.StatusConflict,
			"workflow_run_project_archived",
			"archived project cannot own a workflow run",
		)
	case errors.As(err, &unavailable):
		return workflowError(
			c,
			http.StatusServiceUnavailable,
			"workflow_run_project_unavailable",
			"project is unavailable for attributed workflow run",
		)
	default:
		return workflowStoreFailure(c, err)
	}
}

func (s *Server) workflowManualRunLead(
	ctx context.Context,
	admitted snapshot.TeamRunSnapshot,
) (string, int, error) {
	artifact, err := s.Workflow.GetArtifact(
		ctx, admitted.WorkspaceID, admitted.WorkflowID, admitted.WorkflowVersion,
	)
	if err != nil {
		return "", 0, fmt.Errorf("read admitted manual workflow artifact: %w", err)
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(frozen.ArtifactEnvelopeV1{
		WorkspaceID:               artifact.WorkspaceID,
		WorkflowID:                artifact.WorkflowID,
		WorkflowVersion:           artifact.WorkflowVersion,
		ArtifactSchemaVersion:     artifact.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: artifact.CanonicalizationAlgorithm,
		CanonicalizationVersion:   artifact.CanonicalizationVersion,
		HashAlgorithm:             artifact.HashAlgorithm,
		ContentHash:               artifact.ContentHash,
		Payload:                   artifact.Payload,
	})
	if err != nil {
		return "", 0, fmt.Errorf("decode admitted manual workflow artifact: %w", err)
	}
	if payload.Team.WorkspaceID != admitted.WorkspaceID ||
		payload.Team.TeamID != admitted.TeamID ||
		payload.Team.LeadAgentID == "" {
		return "", 0, errors.New("admitted manual workflow artifact has mismatched lead identity")
	}
	if payload.Team.LeadAgentVersion > 0 {
		return payload.Team.LeadAgentID, int(payload.Team.LeadAgentVersion), nil
	}
	for _, bundle := range payload.Bundles {
		if bundle.Agent.AgentID == payload.Team.LeadAgentID && bundle.Agent.AgentVersion > 0 {
			return payload.Team.LeadAgentID, int(bundle.Agent.AgentVersion), nil
		}
	}
	return "", 0, errors.New("admitted manual workflow artifact has no frozen lead bundle")
}

func (s *Server) respondWorkflowManualRunAdmissionError(
	c echo.Context,
	tx pgx.Tx,
	workflowID string,
	err error,
	triggerType ...string,
) error {
	ctx := c.Request().Context()
	var denial *workflow.FixedWorkflowAdmissionDenial
	if errors.As(err, &denial) {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil &&
			!errors.Is(rollbackErr, pgx.ErrTxClosed) {
			return workflowStoreFailure(c, errors.Join(err, rollbackErr))
		}
		auditTrigger := "manual"
		if len(triggerType) > 0 && triggerType[0] != "" {
			auditTrigger = triggerType[0]
		}
		record, auditErr := s.Workflow.RecordFixedWorkflowAdmissionDenial(
			context.WithoutCancel(ctx),
			workflow.FixedWorkflowAdmissionDenialAttempt{
				WorkspaceID:         getTenant(c),
				WorkflowID:          workflowID,
				WorkflowVersion:     denial.WorkflowVersion,
				TriggerType:         auditTrigger,
				AdmissionAttemptKey: uuid.NewString(),
				ReasonCode:          denial.ReasonCode,
			},
		)
		if auditErr != nil {
			return workflowStoreFailure(c, errors.Join(err, auditErr))
		}
		return workflowError(
			c,
			http.StatusConflict,
			string(record.ReasonCode),
			"workflow run admission denied",
		)
	}

	switch {
	case errors.Is(err, workflow.ErrNotFound):
		return workflowError(c, http.StatusNotFound, "workflow_not_found", "workflow not found")
	case errors.Is(err, workflow.ErrArchived):
		return workflowError(c, http.StatusConflict, "workflow_archived", "workflow archived")
	case errors.Is(err, workflow.ErrNotPublished):
		return workflowError(
			c,
			http.StatusUnprocessableEntity,
			"workflow_not_published",
			"workflow is not published",
		)
	case errors.Is(err, workflow.ErrWorkflowScheduleAdmissionDenied):
		return workflowError(
			c,
			http.StatusUnprocessableEntity,
			"workflow_not_runnable",
			"workflow is not runnable",
		)
	default:
		return workflowStoreFailure(c, err)
	}
}

func validateWorkflowManualRunSnapshot(
	admitted snapshot.TeamRunSnapshot,
	workspaceID, workflowID, sourceRef string,
	triggerTypes ...string,
) error {
	if admitted.RunID == "" ||
		admitted.WorkspaceID != workspaceID ||
		admitted.TeamID == "" ||
		admitted.SnapshotSchemaVersion != 2 ||
		admitted.Mode != "fixed_workflow" ||
		admitted.WorkflowID != workflowID ||
		admitted.WorkflowVersion < 1 ||
		admitted.ArtifactWorkflowID != workflowID ||
		admitted.ArtifactWorkflowVersion != admitted.WorkflowVersion {
		return errors.New("manual admission returned a mismatched fixed workflow identity")
	}
	var trigger struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		SourceRef     string `json:"source_ref"`
	}
	if err := decodeExactJSON(admitted.TriggerSourceV2, &trigger); err != nil {
		return fmt.Errorf("decode manual trigger source: %w", err)
	}
	expectedTrigger := "manual"
	if len(triggerTypes) > 0 && triggerTypes[0] != "" {
		expectedTrigger = triggerTypes[0]
	}
	if trigger.SchemaVersion != 1 || trigger.Type != expectedTrigger ||
		trigger.SourceRef != sourceRef {
		return errors.New("manual admission returned a mismatched trigger")
	}
	return nil
}
