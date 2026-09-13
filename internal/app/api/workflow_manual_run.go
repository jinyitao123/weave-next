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
	"github.com/jinyitao123/weave/internal/app/workflowdispatch"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

type workflowManualRunResponse struct {
	RunID           string `json:"run_id"`
	InputRevisionID string `json:"input_revision_id,omitempty"`
	ClientRequestID string `json:"client_request_id,omitempty"`
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion int    `json:"workflow_version"`
	TaskID          string `json:"task_id"`
	ProjectID       string `json:"project_id,omitempty"`
	ConversationID  string `json:"conversation_id,omitempty"`
}

type workflowDispatchIdentity struct {
	RunID      string
	TaskID     string
	ContextKey string
}

func (s *Server) workflowDispatchService() *workflowdispatch.Service {
	return &workflowdispatch.Service{
		Workflow: s.Workflow, Snapshots: s.Snapshots, Tasks: s.Tasks,
		Deliverables: s.Deliverables,
	}
}

// admitTeamWorkflowDispatch freezes and enqueues the team dispatch already validated by the product boundary.
func (s *Server) admitTeamWorkflowDispatch(
	c echo.Context,
	workflowID string,
	request teamDispatchRequest,
	identity workflowDispatchIdentity,
) error {
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
	if handled, err := s.lockBoundDispatchInput(c, tx, request); handled || err != nil {
		return err
	}

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
	service := s.workflowDispatchService()
	prepared, err := service.PrepareTx(ctx, tx, workflowdispatch.AdmissionRequest{
		WorkspaceID:     workspaceID,
		WorkflowID:      workflowID,
		WorkflowVersion: request.WorkflowVersion,
		SourceRef:       sourceRef,
		TriggerType:     triggerType,
		RunID:           identity.RunID,
	})
	if err != nil {
		return s.respondWorkflowManualRunAdmissionError(c, tx, workflowID, err, expectedTrigger)
	}
	admitted := prepared.Snapshot()
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
		leadAvatarID, _, err := service.Lead(ctx, admitted)
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
	}
	deliveryContract := json.RawMessage(nil)
	if request.inputBinding != nil {
		deliveryContract = request.inputBinding.DeliveryContract
	}
	receipt, err := service.PersistTx(ctx, tx, prepared, workflowdispatch.PersistRequest{
		ProjectID: projectID, TaskID: identity.TaskID, TaskSource: taskSource,
		ContextKey: identity.ContextKey, Payload: payload,
		InputRevisionID: request.InputRevisionID, DeliveryContract: deliveryContract,
	})
	if err != nil {
		if errors.Is(err, snapshot.ErrAlreadyExists) {
			if identity.ContextKey != "" {
				_ = tx.Rollback(ctx)
				existing, _, existingPayload, contextKey, found, replayErr := s.loadWorkflowDispatchReplay(
					ctx, workspaceID, identity.RunID, identity.TaskID,
				)
				if replayErr != nil {
					return workflowStoreFailure(c, replayErr)
				}
				if found && contextKey == identity.ContextKey && existingPayload == string(payload) {
					existing.RunID = identity.RunID
					existing.ConversationID = conversationID
					return c.JSON(http.StatusOK, existing)
				}
				return workflowError(c, http.StatusConflict, "client_request_conflict", "client_request_id was already used for different dispatch facts")
			}
		}
		return workflowStoreFailure(c, err)
	}
	if err := consumeDispatchInputTx(ctx, tx, workspaceID, getUserID(c), request.InputRevisionID, receipt.RunID, receipt.TaskID); err != nil {
		return workflowStoreFailure(c, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("commit manual workflow run: %w", err))
	}

	response := workflowManualRunResponse{
		RunID:           receipt.RunID,
		WorkflowID:      receipt.WorkflowID,
		WorkflowVersion: receipt.WorkflowVersion,
		TaskID:          receipt.TaskID,
		ProjectID:       receipt.ProjectID,
		ConversationID:  conversationID,
		InputRevisionID: request.InputRevisionID,
	}
	if request.InputRevisionID != "" {
		response.ClientRequestID = request.ClientRequestID
	}
	return c.JSON(http.StatusCreated, response)
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
