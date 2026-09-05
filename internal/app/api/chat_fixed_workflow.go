package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/labstack/echo/v4"
)

var errPublishedConversationWorkflowAmbiguous = errors.New("multiple published conversation workflows are available")

func runtimeInferenceTeamGraphUnavailable(
	runtimeInferenceSelected bool,
	publishedWorkflowID string,
	teamExecution *teamSessionExecution,
) bool {
	// A published workflow owns admission, execution state, and worker routing;
	// it intentionally does not acquire the legacy Loom team session graph.
	return runtimeInferenceSelected && publishedWorkflowID == "" && teamExecution == nil
}

func publishedWorkflowRunContext(requestCtx context.Context) context.Context {
	return context.WithoutCancel(requestCtx)
}

const (
	publishedWorkflowContextMessages = 12
	publishedWorkflowContextChars    = 48000
)

// publishedWorkflowConversationInput preserves ordinary follow-up semantics
// for a team workflow while keeping the latest user request authoritative.
// Initial turns remain byte-for-byte unchanged; later turns receive a bounded
// transcript so references such as "revise the previous delivery" are usable.
func publishedWorkflowConversationInput(history []contract.Message, latest string) string {
	latest = strings.TrimSpace(latest)
	if len(history) <= 1 {
		return latest
	}
	previous := history[:len(history)-1]
	if len(previous) > publishedWorkflowContextMessages {
		previous = previous[len(previous)-publishedWorkflowContextMessages:]
	}
	remaining := publishedWorkflowContextChars - len(latest)
	if remaining <= 0 {
		return latest
	}
	parts := make([]string, 0, len(previous))
	for index := len(previous) - 1; index >= 0 && remaining > 0; index-- {
		content := strings.TrimSpace(previous[index].Content)
		if content == "" {
			continue
		}
		role := "assistant"
		if previous[index].Role == "user" {
			role = "user"
		}
		part := "[" + role + "]\n" + content
		if len(part) > remaining {
			part = part[len(part)-remaining:]
		}
		parts = append(parts, part)
		remaining -= len(part)
	}
	if len(parts) == 0 {
		return latest
	}
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	return "Conversation context for continuity only; the latest user request below has highest priority.\n\n" +
		strings.Join(parts, "\n\n") +
		"\n\nLatest user request:\n" + latest
}

// publishedConversationWorkflowForLead resolves the production entry point for
// an active team. A chat message may select a workflow implicitly only when the
// team has exactly one published conversation_explicit workflow.
func (s *Server) publishedConversationWorkflowForLead(
	ctx context.Context,
	workspaceID, leadAvatarID string,
) (string, error) {
	if s.StoreExt == nil || s.Workflow == nil {
		return "", nil
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		return "", fmt.Errorf("begin published conversation workflow lookup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT workflow.id
		FROM weave_teams AS team
		JOIN weave_team_workflows AS workflow
		  ON workflow.workspace_id=team.workspace_id AND workflow.team_id=team.id
		JOIN weave_team_workflow_versions AS version
		  ON version.workspace_id=workflow.workspace_id
		 AND version.workflow_id=workflow.id
		 AND version.version=workflow.published_version
		WHERE team.workspace_id=$1 AND team.lead_avatar_id=$2
		  AND team.status='active' AND workflow.status='active'
		  AND version.status='published'
		  AND version.trigger_config->>'type'='conversation_explicit'
		ORDER BY workflow.id
		LIMIT 2
	`, workspaceID, leadAvatarID)
	if err != nil {
		return "", fmt.Errorf("resolve published conversation workflow: %w", err)
	}
	defer rows.Close()
	workflowIDs := make([]string, 0, 2)
	for rows.Next() {
		var workflowID string
		if err := rows.Scan(&workflowID); err != nil {
			return "", fmt.Errorf("decode published conversation workflow: %w", err)
		}
		workflowIDs = append(workflowIDs, workflowID)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate published conversation workflows: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("finish published conversation workflow lookup: %w", err)
	}
	switch len(workflowIDs) {
	case 0:
		return "", nil
	case 1:
		return workflowIDs[0], nil
	default:
		return "", errPublishedConversationWorkflowAmbiguous
	}
}

func (s *Server) admitPublishedWorkflowChat(
	ctx context.Context,
	workspaceID, workflowID, projectID, conversationID, message string,
) (snapshot.TeamRunSnapshot, string, error) {
	if s.Workflow == nil || s.ScheduleTransactions == nil || s.Snapshots == nil || s.Tasks == nil {
		return snapshot.TeamRunSnapshot{}, "", errors.New("workflow run service unavailable")
	}
	tx, err := s.ScheduleTransactions.Begin(ctx)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, "", fmt.Errorf("begin conversation workflow run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	admitted, err := s.Workflow.AdmitWorkflowManualRunTx(ctx, tx, workflow.WorkflowManualRunAdmissionRequest{
		WorkspaceID: workspaceID,
		WorkflowID:  workflowID,
		SourceRef:   conversationID,
		TriggerType: "conversation_explicit",
	})
	if err != nil {
		return snapshot.TeamRunSnapshot{}, "", fmt.Errorf("admit conversation workflow run: %w", err)
	}
	if err := validateWorkflowManualRunSnapshot(
		admitted, workspaceID, workflowID, conversationID, "conversation_explicit",
	); err != nil {
		return snapshot.TeamRunSnapshot{}, "", err
	}
	project, err := loadWorkflowManualRunProject(ctx, tx, workspaceID, projectID)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, "", err
	}
	leadAvatarID, _, err := s.workflowManualRunLead(ctx, admitted)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, "", err
	}
	if project.AvatarID != leadAvatarID {
		return snapshot.TeamRunSnapshot{}, "", errors.New("project avatar does not match admitted workflow lead avatar")
	}
	var conversationProjectID, conversationAgentID string
	if err := tx.QueryRow(ctx, `
		SELECT project_id,agent_id FROM weave_conversations
		WHERE workspace_id=$1 AND id=$2 AND parent_message_id IS NULL
		FOR SHARE
	`, workspaceID, conversationID).Scan(&conversationProjectID, &conversationAgentID); err != nil {
		return snapshot.TeamRunSnapshot{}, "", fmt.Errorf("read conversation workflow conversation: %w", err)
	}
	if conversationProjectID != projectID || conversationAgentID != leadAvatarID {
		return snapshot.TeamRunSnapshot{}, "", errors.New("conversation does not match admitted workflow project and lead")
	}
	admitted.ProjectID = project.ID
	created, err := s.Snapshots.CreateTx(ctx, tx, admitted)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, "", fmt.Errorf("create conversation workflow snapshot: %w", err)
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return snapshot.TeamRunSnapshot{}, "", fmt.Errorf("encode conversation workflow input: %w", err)
	}
	task := newPublishedWorkflowChatTask(created, payload)
	if err := s.Tasks.EnqueueTx(ctx, tx, task); err != nil {
		return snapshot.TeamRunSnapshot{}, "", fmt.Errorf("enqueue conversation workflow: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return snapshot.TeamRunSnapshot{}, "", fmt.Errorf("commit conversation workflow run: %w", err)
	}
	return *created, task.ID, nil
}

func newPublishedWorkflowChatTask(created *snapshot.TeamRunSnapshot, payload json.RawMessage) *taskqueue.Task {
	return &taskqueue.Task{
		ID:                    "task-" + uuid.NewString(),
		WorkspaceID:           created.WorkspaceID,
		ProjectID:             created.ProjectID,
		IdentityKind:          taskqueue.IdentityTeamWorkflow,
		IdentitySchemaVersion: 2,
		WorkflowID:            created.WorkflowID,
		WorkflowVersion:       created.WorkflowVersion,
		RunSnapshotID:         created.RunID,
		Source:                "session",
		Kind:                  "team_workflow",
		Payload:               payload,
	}
}

func decodePublishedWorkflowChatOutput(raw json.RawMessage) (string, error) {
	var envelope struct {
		Output json.RawMessage `json:"output"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || len(envelope.Output) == 0 {
		return "", errors.New("workflow terminal task has no output")
	}
	var text string
	if err := json.Unmarshal(envelope.Output, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return "", errors.New("workflow terminal output is blank")
		}
		return text, nil
	}
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, envelope.Output, "", "  "); err != nil {
		return "", fmt.Errorf("decode workflow terminal output: %w", err)
	}
	return formatted.String(), nil
}

func (s *Server) awaitPublishedWorkflowChat(
	ctx context.Context,
	workspaceID, runSnapshotID string,
) (string, error) {
	// The TeamRun owns the execution budget for every node and loop iteration.
	// Do not impose a shorter, independent wall-clock deadline here: a healthy
	// published workflow can legitimately outlive a single-node budget while
	// the durable run continues toward a terminal result.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		var status string
		var errorCode *string
		tx, err := s.StoreExt.BeginTx(ctx)
		if err != nil {
			return "", fmt.Errorf("begin conversation workflow status read: %w", err)
		}
		err = tx.QueryRow(ctx, `
			SELECT status,error_code FROM weave_team_runs
			WHERE workspace_id=$1 AND run_snapshot_id=$2
		`, workspaceID, runSnapshotID).Scan(&status, &errorCode)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			return "", fmt.Errorf("read conversation workflow run: %w", err)
		}
		switch status {
		case "succeeded":
			var result json.RawMessage
			err := tx.QueryRow(ctx, `
				SELECT result FROM weave_task_queue
				WHERE workspace_id=$1 AND run_snapshot_id=$2
				  AND status='completed' AND jsonb_typeof(result)='object'
				  AND result ? 'output'
				ORDER BY completed_at DESC,id DESC LIMIT 1
			`, workspaceID, runSnapshotID).Scan(&result)
			if err == nil {
				if err := tx.Commit(ctx); err != nil {
					return "", fmt.Errorf("finish conversation workflow output read: %w", err)
				}
				return decodePublishedWorkflowChatOutput(result)
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				_ = tx.Rollback(ctx)
				return "", fmt.Errorf("read conversation workflow output: %w", err)
			}
		case "failed", "cancelled", "abandoned":
			code := "team workflow " + status
			if errorCode != nil && strings.TrimSpace(*errorCode) != "" {
				code += ": " + *errorCode
			}
			_ = tx.Rollback(ctx)
			return "", errors.New(code)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("finish conversation workflow status read: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Server) respondPublishedWorkflowChat(
	c echo.Context,
	req ChatRequest,
	rec *registry.AgentRecord,
	workspaceID, userID, sessionKey, conversationID, userMessageID, workflowID string,
	history []contract.Message,
) error {
	requestCtx := c.Request().Context()
	// A published team workflow is admitted into durable server-owned state.
	// Disconnecting the HTTP/SSE reader must stop delivery only; the workflow,
	// terminal persistence, and conversation write continue on the same request.
	runCtx := publishedWorkflowRunContext(requestCtx)
	failRequest := func(runID string, cause error) {
		s.finishChatRequest(
			runCtx, workspaceID, userID, req.ClientRequestID,
			"failed", runID, map[string]any{
				"project_id": req.ProjectID, "conversation_id": conversationID,
				"user_message_id": userMessageID, "error": cause.Error(),
			},
		)
	}
	created, taskID, err := s.admitPublishedWorkflowChat(
		runCtx, workspaceID, workflowID, req.ProjectID, conversationID,
		publishedWorkflowConversationInput(history, req.Message),
	)
	if err != nil {
		failRequest("", err)
		return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
	}
	if s.ChatRequests != nil && strings.TrimSpace(req.ClientRequestID) != "" {
		if _, err := s.ChatRequests.BindWorkflowRun(
			runCtx, workspaceID, userID, req.ClientRequestID, created.RunID, taskID,
		); err != nil {
			failRequest(created.RunID, err)
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_workflow_binding_failed"})
		}
	}
	var sse *SSEWriter
	if req.Stream {
		sse, err = NewSSEWriter(c)
		if err != nil {
			failRequest(created.RunID, err)
			return err
		}
		_ = sse.SendEvent("step_start", map[string]any{
			"step": "published_workflow", "run_id": created.RunID,
		})
	}
	output, runErr := s.awaitPublishedWorkflowChat(runCtx, workspaceID, created.RunID)
	if runErr != nil {
		payload := map[string]any{
			"session_id": req.SessionID, "run_id": created.RunID,
			"project_id": req.ProjectID, "conversation_id": conversationID,
			"user_message_id": userMessageID, "error": runErr.Error(),
		}
		failRequest(created.RunID, runErr)
		if sse != nil {
			_ = sse.SendEvent("done", payload)
			return nil
		}
		return c.JSON(http.StatusInternalServerError, payload)
	}

	assistant := contract.Message{Role: "assistant", Content: output}
	_ = s.appendSessionMessages(runCtx, sessionKey, assistant)
	metadata := s.buildAssistantMetadata(contextWithAssistantAgent(runCtx, rec.Name), workspaceID, output)
	metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
	if _, err := s.Conversations.AppendMessage(runCtx, conversation.Message{
		ConversationID: conversationID,
		WorkspaceID:    workspaceID,
		Role:           "assistant",
		Content:        output,
		Metadata:       metadata,
	}); err != nil {
		failRequest(created.RunID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	resp := ChatResponse{
		Output: output, StopReason: "completed", SessionID: req.SessionID,
		RunID: created.RunID, ProjectID: req.ProjectID,
		ConversationID: conversationID, UserMessageID: userMessageID,
		RuntimeAssignment: req.RuntimeAssignment,
	}
	s.finishChatRequest(runCtx, workspaceID, userID, req.ClientRequestID, "completed", created.RunID, resp)
	if sse != nil {
		_ = sse.SendEvent("chunk", map[string]any{"agent": rec.Name, "content": output})
		_ = sse.SendEvent("done", resp)
		return nil
	}
	return c.JSON(http.StatusOK, resp)
}
