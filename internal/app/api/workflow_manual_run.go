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
	"github.com/jinyitao123/weave/internal/app/workflowadmission"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/publication"
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

// admitTeamWorkflowDispatch freezes and enqueues the team dispatch already validated by the product boundary.
func (s *Server) admitTeamWorkflowDispatch(c echo.Context, workflowID string, request teamDispatchRequest) error {
	if s.Workflow == nil || s.ScheduleTransactions == nil ||
		s.workflowAdmissions() == nil {
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
	envelope, err := s.Workflow.ResolvePublished(ctx, workspaceID, workflowID, request.WorkflowVersion)
	if err != nil {
		return s.respondWorkflowManualRunAdmissionError(c, nil, workflowID, err)
	}
	frozenPayload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
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
	if conversationID != "" {
		sourceRef = conversationID
		triggerType = "conversation_explicit"
		expectedTrigger = triggerType
	}
	admitted, err := s.Workflow.PrepareWorkflowManualRunTx(
		ctx,
		tx,
		workflow.WorkflowManualRunAdmissionRequest{
			WorkspaceID:     workspaceID,
			WorkflowID:      workflowID,
			WorkflowVersion: request.WorkflowVersion,
			SourceRef:       sourceRef,
			TriggerType:     triggerType,
		}, envelope,
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
			WHERE workspace_id=$1 AND id=$2 AND user_id=$3 AND parent_message_id IS NULL
			FOR SHARE
		`, workspaceID, conversationID, getUserID(c)).Scan(&conversationProjectID, &conversationAgentID); errors.Is(err, pgx.ErrNoRows) {
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
		leadAvatarID := frozenPayload.Team.LeadAgentID
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

	taskID := "task-" + uuid.NewString()
	if dispatchTaskID, ok := c.Get("workflow_dispatch_task_id").(string); ok && dispatchTaskID != "" {
		taskID = dispatchTaskID
	}
	contextKey, _ := c.Get("workflow_dispatch_fingerprint").(string)
	inputVersion := request.InputRevisionID
	if inputVersion == "" {
		inputVersion = "sha256:" + dispatchInputDigest(payload)
	}
	contract, err := dispatchRequestedDeliveryContract(request)
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	intent := publication.PublishedRunRequest{Version: publication.ContractVersion, RequestID: admitted.RunID,
		Revision: publication.CandidateRevision(envelope), RunID: admitted.RunID, TaskID: taskID,
		Input: payload, InputVersion: inputVersion, ProjectID: admitted.ProjectID, ContextKey: contextKey,
		Trigger: publication.PublishedTrigger{Type: expectedTrigger, SourceRef: sourceRef}, DeliveryContract: contract}
	_, err = s.workflowAdmissions().ReserveTx(ctx, tx, intent, workflowadmission.Target{TeamID: admitted.TeamID, InputRevisionID: request.InputRevisionID, ConversationID: conversationID})
	if err != nil {
		return workflowStoreFailure(c, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return workflowStoreFailure(c, fmt.Errorf("commit workflow admission intent: %w", err))
	}
	return s.finishWorkflowAdmission(c, intent.RequestID, request.ClientRequestID, http.StatusCreated)
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
		if tx != nil {
			if rollbackErr := tx.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				return workflowStoreFailure(c, errors.Join(err, rollbackErr))
			}
		}

		auditTrigger := "manual"
		if len(triggerType) > 0 && triggerType[0] != "" {
			auditTrigger = triggerType[0]
		}
		record, auditErr := s.WorkflowArtifacts.RecordFixedWorkflowAdmissionDenial(
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
	case errors.Is(err, publication.ErrRequestConflict):
		return workflowError(c, http.StatusConflict, "workflow_request_conflict", "workflow request no longer matches current authorized input")
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
