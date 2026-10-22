package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/app/workflowadmission"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/labstack/echo/v4"
)

func (s *Server) workflowAdmissions() *workflowadmission.Store {
	kernel, ok := s.KernelPublication.(publication.PublishedService)
	if !ok || s.GetPool() == nil {
		return nil
	}
	return workflowadmission.New(s.GetPool(), kernel)
}

func (s *Server) associateWorkflowAdmission(ctx context.Context, tx pgx.Tx, record workflowadmission.Record) error {
	if record.Receipt == nil {
		return publication.ErrInvalidReceipt
	}
	if record.Target.ScheduleID != "" {
		return s.associateScheduledAdmission(ctx, tx, record)
	}
	if record.Target.ChatRequestID != "" {
		tag, err := tx.Exec(ctx, `UPDATE weave_chat_requests SET run_id=$4,task_id=$5,status='running',updated_at=now()
   WHERE workspace_id=$1 AND user_id=$2 AND client_request_id=$3 AND user_message_id=$6
    AND status IN ('admitted','queued','running') AND (run_id IS NULL OR run_id=$4)`, record.Subject.WorkspaceID, record.Subject.UserID, record.Target.ChatRequestID, record.Receipt.RunID, record.Receipt.TaskID, record.Request.InputVersion)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return publication.ErrRequestConflict
		}
		return nil
	}
	return consumeDispatchInputTx(ctx, tx, record.Subject.WorkspaceID, record.Subject.UserID, record.Target.InputRevisionID, record.Receipt.RunID, record.Receipt.TaskID)
}

func workflowAdmissionResponse(record workflowadmission.Record, clientRequestID string) workflowManualRunResponse {
	response := workflowManualRunResponse{RunID: record.Receipt.RunID, WorkflowID: record.Receipt.Revision.WorkflowID, WorkflowVersion: record.Receipt.Revision.WorkflowVersion,
		TaskID: record.Receipt.TaskID, ProjectID: record.Request.ProjectID, ConversationID: record.Target.ConversationID, InputRevisionID: record.Target.InputRevisionID}
	if record.Target.InputRevisionID != "" {
		response.ClientRequestID = clientRequestID
	}
	return response
}

func (s *Server) finishWorkflowAdmission(c echo.Context, requestID, clientRequestID string, status int) error {
	store := s.workflowAdmissions()
	if store == nil {
		return workflowError(c, http.StatusServiceUnavailable, "workflow_run_unavailable", "workflow run service unavailable")
	}
	record, err := store.Admit(c.Request().Context(), getTenant(c), requestID, s.associateWorkflowAdmission)
	if errors.Is(err, publication.ErrAdmissionClosed) {
		return workflowError(c, http.StatusConflict, "dispatch_input_closed", "dispatch was closed before admission")
	}
	if err != nil {
		stored, readErr := store.Get(c.Request().Context(), getTenant(c), requestID)
		if readErr == nil {
			return s.respondWorkflowManualRunAdmissionError(c, nil, stored.Request.Revision.WorkflowID, err, stored.Request.Trigger.Type)
		}
		return workflowStoreFailure(c, err)
	}
	return c.JSON(status, workflowAdmissionResponse(record, clientRequestID))
}
