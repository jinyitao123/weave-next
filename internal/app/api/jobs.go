package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/sessionexec"
	"github.com/labstack/echo/v4"
)

var validJobStatuses = map[string]struct{}{
	taskqueue.StatusQueued:     {},
	taskqueue.StatusDispatched: {},
	taskqueue.StatusRunning:    {},
	taskqueue.StatusCompleted:  {},
	taskqueue.StatusFailed:     {},
	taskqueue.StatusCancelled:  {},
	taskqueue.StatusSuperseded: {},
	taskqueue.StatusCut:        {},
	taskqueue.StatusTimedOut:   {},
}

func taskRuntimeAssignment(assignment *runtimes.Assignment) *taskqueue.RuntimeAssignment {
	if assignment == nil {
		return nil
	}
	facts := make([]taskqueue.RuntimeCapabilityFact, 0, len(assignment.CapabilityFacts))
	for _, fact := range assignment.CapabilityFacts {
		facts = append(facts, taskqueue.RuntimeCapabilityFact{
			RuntimeID: fact.RuntimeID, Name: fact.Name,
			Engines:         append([]string(nil), fact.Engines...),
			RuntimeRevision: fact.RuntimeRevision,
			Enabled:         fact.Enabled, Online: fact.Online, Eligible: fact.Eligible,
			UnavailableReason: fact.UnavailableReason,
		})
	}
	return &taskqueue.RuntimeAssignment{
		RuntimeID: assignment.RuntimeID, RuntimeRevision: assignment.RuntimeRevision,
		Mode: assignment.Mode, ReasonCode: assignment.ReasonCode, Engine: assignment.Engine,
		CapabilityFacts: facts,
	}
}

func (s *Server) applyTaskRuntimeAssignment(
	ctx context.Context,
	workspaceID string,
	rec *registry.AgentRecord,
	assignment *taskqueue.RuntimeAssignment,
) (*registry.AgentRecord, error) {
	if assignment == nil {
		return rec, nil
	}
	if rec == nil {
		return nil, errors.New("runtime assignment agent is unavailable")
	}
	engineName := rec.Engine
	if engineName == "" {
		engineName = "loom"
	}
	if assignment.Engine != engineName {
		return nil, errors.New("runtime assignment engine differs from frozen agent")
	}
	if !engine.IsCLIEngine(rec.Engine) {
		if assignment.RuntimeID != "" || assignment.ReasonCode != "in_process_engine" {
			return nil, errors.New("in-process runtime assignment is invalid")
		}
		return rec, nil
	}
	if assignment.RuntimeID == "" || assignment.RuntimeRevision < 1 || s.Runtimes == nil {
		return nil, runtimes.ErrRuntimeSelectionUnavailable
	}
	selected, err := s.Runtimes.Get(ctx, workspaceID, assignment.RuntimeID)
	if err != nil || !selected.Online || selected.FunctionalRevision != assignment.RuntimeRevision {
		return nil, runtimes.ErrRuntimeSelectionUnavailable
	}
	engineAvailable := false
	for _, candidate := range selected.Engines {
		if candidate == rec.Engine {
			engineAvailable = true
			break
		}
	}
	if !engineAvailable {
		return nil, runtimes.ErrRuntimeSelectionUnavailable
	}
	resolved := *rec
	resolved.RuntimeID = assignment.RuntimeID
	return &resolved, nil
}

type chatAgentRegistry interface {
	Get(ctx context.Context, tenant, name string) (*registry.AgentRecord, error)
	GetVersion(ctx context.Context, tenant, agentID string, version int) (*registry.AgentRecord, error)
}

func resolveChatAgent(
	ctx context.Context,
	reg chatAgentRegistry,
	tenant string,
	req *taskqueue.ChatExecRequest,
) (*registry.AgentRecord, error) {
	if req == nil {
		return nil, fmt.Errorf("chat execution request is required")
	}
	stamp := req.ExecutionStamp
	if stamp == nil {
		rec, err := reg.Get(ctx, tenant, req.Agent)
		return rec, err
	}
	if stamp.AgentID == "" || stamp.AgentVersion < 1 || !stamp.ExecutionScope.Valid() {
		return nil, fmt.Errorf("invalid agent execution stamp")
	}
	if stamp.LegacyScope && stamp.ExecutionScope != execution.ScopeLegacyOrchestrator {
		return nil, fmt.Errorf("legacy execution stamp must use legacy orchestrator scope")
	}
	rec, err := reg.GetVersion(ctx, tenant, stamp.AgentID, stamp.AgentVersion)
	if err != nil {
		return nil, err
	}
	if rec.ID != stamp.AgentID || rec.Version != stamp.AgentVersion ||
		rec.WorkspaceID != tenant || rec.Name == "" || rec.Name != req.Agent {
		return nil, fmt.Errorf(
			"frozen agent identity does not match %q@%d for agent %q",
			stamp.AgentID, stamp.AgentVersion, req.Agent,
		)
	}
	if stamp.ExecutionScope == execution.ScopeTeamWorkerLeaf {
		if err := registry.ValidateTeamWorkerAgentRecord(rec); err != nil {
			return nil, err
		}
	}
	return rec, nil
}

func chatUsesNameResolver(stamp *execution.AgentExecutionStamp, noDispatch bool) bool {
	if noDispatch {
		return false
	}
	return stamp == nil || stamp.ExecutionScope == execution.ScopeLegacyOrchestrator
}

func (s *Server) loadAsyncTeamSessionExecution(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	req taskqueue.ChatExecRequest,
) (*teamSessionExecution, error) {
	if req.RunSnapshotID == "" {
		return nil, nil
	}
	if s.StoreExt == nil || s.Snapshots == nil || rec == nil {
		return nil, fmt.Errorf("team job session dependencies are unavailable")
	}
	snap, err := s.Snapshots.GetByRunID(ctx, tenant, req.RunSnapshotID)
	if err != nil {
		return nil, err
	}
	if snap.Mode != "free_collab" || snap.RunID != req.RunSnapshotID ||
		snap.LeadAvatarID != rec.ID || snap.LeadAvatarVersion != rec.Version {
		return nil, fmt.Errorf("team job snapshot identity mismatch")
	}
	var frozenRuntimeAssignment taskqueue.RuntimeAssignment
	if len(snap.RuntimeAssignment) == 0 {
		if req.RuntimeAssignment != nil {
			return nil, fmt.Errorf("team job runtime assignment mismatch")
		}
	} else {
		if err := json.Unmarshal(snap.RuntimeAssignment, &frozenRuntimeAssignment); err != nil {
			return nil, fmt.Errorf("decode team job runtime assignment: %w", err)
		}
		if !taskRuntimeAssignmentsEqual(&frozenRuntimeAssignment, req.RuntimeAssignment) {
			return nil, fmt.Errorf("team job runtime assignment mismatch")
		}
	}
	key := sessionexec.SessionKey{
		WorkspaceID: tenant, UserID: req.UserID,
		LeadAvatarID: rec.ID, SessionID: req.SessionID,
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lease, err := sessionexec.NewPGStore().Get(ctx, tx, key)
	if err != nil {
		return nil, err
	}
	if lease.ActiveRunID != snap.RunID || lease.RunSnapshotID != snap.RunID ||
		lease.State != sessionexec.LeaseStateActive {
		return nil, fmt.Errorf("team job lease identity mismatch")
	}
	attribution, err := terminalAttributionFromSnapshot(tenant, *snap, nil)
	if err != nil {
		return nil, err
	}
	return &teamSessionExecution{
		Lease: lease, Snapshot: *snap, TerminalAttribution: attribution,
		AcquireEventID: "job:" + snap.RunID,
		LoomSessionKey: tenant + ":" + req.UserID + ":" + rec.Name + ":" + req.SessionID,
		ConversationID: req.ConversationID, NextYieldGeneration: 1,
		memoryOutboxReady: make(chan struct{}),
	}, nil
}

func parseJobStatuses(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	statuses := strings.Split(raw, ",")
	for _, status := range statuses {
		if _, ok := validJobStatuses[status]; !ok {
			return nil, fmt.Errorf("invalid status %q", status)
		}
	}
	return statuses, nil
}

// enqueueTask creates an asynchronous chat task and returns 202.
func (s *Server) enqueueTask(
	c echo.Context,
	tenant string,
	req ChatRequest,
	attachments []taskqueue.Attachment,
	resolved ...*registry.AgentRecord,
) error {
	taskID := "task-" + uuid.NewString()
	hadSessionID := req.SessionID != ""
	userID := getUserID(c)
	var rec *registry.AgentRecord
	if len(resolved) > 0 {
		rec = resolved[0]
	}
	if rec == nil {
		if s.Registry == nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{
				"error": "agent registry is unavailable",
			})
		}
		var err error
		rec, err = s.Registry.Get(c.Request().Context(), tenant, req.Agent)
		if err != nil {
			return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
		}
	}
	if req.ConversationID != "" {
		if _, err := s.resolveRequestedChatConversation(
			c.Request().Context(), tenant, req.ProjectID, rec.ID, userID, req.ConversationID, req.Channel,
		); err != nil {
			return respondChatConversationError(c, err)
		}
	}
	admitted, err := s.admitOrReplayChatRequest(c, c.Request().Context(), tenant, userID, rec.ID, req)
	if err != nil || !admitted {
		return err
	}
	requestFinalized := req.ClientRequestID == ""
	defer func() {
		if !requestFinalized {
			s.finishChatRequest(
				c.Request().Context(), tenant, userID, req.ClientRequestID,
				"failed", "", map[string]any{"project_id": req.ProjectID, "error": "enqueue_failed"},
			)
		}
	}()
	rec, req.RuntimeAssignment, err = s.resolveChatRuntime(
		c.Request().Context(), tenant, req.RuntimeID, rec,
	)
	if err != nil {
		return respondChatRuntimeError(c, err)
	}
	runtimeAssignmentJSON, err := encodeRuntimeAssignment(req.RuntimeAssignment)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "runtime_assignment_encode_failed"})
	}
	if req.SessionID == "" {
		req.SessionID = uuid.NewString()
	}
	sessionKey := tenant + ":" + userID + ":" + req.Agent + ":" + req.SessionID
	var teamExecution *teamSessionExecution
	teamExecutionTransferred := false
	if !teamAssemblerDisabled() {
		teamExecution, err = s.acquireTeamSession(
			c.Request().Context(), rec, tenant, req.ProjectID, runtimeAssignmentJSON, userID, req.SessionID,
			sessionKey, "job:"+taskID,
		)
		if err != nil {
			return c.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
		}
	}
	defer func() {
		if teamExecution == nil || teamExecutionTransferred {
			return
		}
		_ = s.finishTeamSessionExecution(
			context.WithoutCancel(c.Request().Context()), teamExecution,
			loomruntime.Result{}, "", nil, errors.New("async team session admission failed"),
		)
	}()
	conversationID := ""
	userMessageID := ""
	if s.Conversations != nil {
		conversationID, userMessageID, err = s.recordIncomingProductMessage(
			c.Request().Context(), tenant, req.ProjectID, rec.ID, userID, sessionKey, req.Message,
			incomingProductMessageOptions{
				ConversationID: req.ConversationID,
				TeamExecution:  teamExecution,
				Attachments:    attachments,
			},
		)
		if err != nil {
			if req.ConversationID != "" {
				return respondChatConversationError(c, err)
			}
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}
	if req.ClientRequestID != "" {
		if _, err := s.ChatRequests.MarkAdmitted(
			c.Request().Context(), tenant, userID, req.ClientRequestID,
			req.SessionID, conversationID, userMessageID,
		); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_admission_failed"})
		}
	}

	// Serialize the request for later execution.
	execReq := taskqueue.ChatExecRequest{
		Agent:             req.Agent,
		ProjectID:         req.ProjectID,
		RuntimeAssignment: taskRuntimeAssignment(req.RuntimeAssignment),
		ClientRequestID:   req.ClientRequestID,
		SessionID:         req.SessionID,
		ConversationID:    conversationID,
		Message:           req.Message,
		Profile:           req.Profile,
		Effort:            req.Effort,
		UserID:            userID,
		Context:           req.Context,
		Attachments:       attachments,
	}
	reqJSON, _ := json.Marshal(execReq)

	contextKey := ""
	if hadSessionID {
		contextKey = userID + ":" + req.Agent + ":" + req.SessionID
	}
	task := &taskqueue.Task{
		ID:                    taskID,
		WorkspaceID:           tenant,
		ProjectID:             req.ProjectID,
		Agent:                 req.Agent,
		AgentID:               rec.ID,
		AgentVersion:          rec.Version,
		IdentityKind:          taskqueue.IdentityAgent,
		IdentitySchemaVersion: 2,
		ExecutionScope:        execution.ScopeLegacyOrchestrator,
		Source:                "chat",
		RuntimeID:             req.RuntimeAssignment.RuntimeID,
		RuntimeAssignment:     runtimeAssignmentJSON,
		ContextKey:            contextKey,
		Payload:               reqJSON,
	}
	if teamExecution != nil {
		task.ExecutionScope = execution.ScopeTeamFreeCollab
		task.RunSnapshotID = teamExecution.Snapshot.RunID
	}

	if err := s.Tasks.Enqueue(c.Request().Context(), task); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	teamExecutionTransferred = true
	if contextKey != "" {
		if err := s.Tasks.Supersede(c.Request().Context(), tenant, contextKey, taskID); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
	}

	response := map[string]any{
		"id":                 taskID,
		"status":             taskqueue.StatusQueued,
		"project_id":         req.ProjectID,
		"conversation_id":    conversationID,
		"user_message_id":    userMessageID,
		"runtime_assignment": req.RuntimeAssignment,
	}
	if req.ClientRequestID != "" {
		encoded, err := json.Marshal(response)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_response_failed"})
		}
		if _, err := s.ChatRequests.MarkQueued(
			c.Request().Context(), tenant, userID, req.ClientRequestID, taskID, encoded,
		); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": "chat_request_queue_record_failed"})
		}
	}
	requestFinalized = true
	return c.JSON(http.StatusAccepted, response)
}

// handleListJobs returns task ledger entries for the current workspace.
func (s *Server) handleListJobs(c echo.Context) error {
	tenant := getTenant(c)
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	offset, _ := strconv.Atoi(c.QueryParam("offset"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	statuses, err := parseJobStatuses(c.QueryParam("status"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	taskList, total, err := s.Tasks.ListFilteredByProject(
		c.Request().Context(), tenant, c.QueryParam("project_id"), statuses, limit, offset,
	)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, taskqueue.TaskListResponse{Jobs: taskList, Total: total})
}

// handleGetJob returns a single task ledger entry by ID.
func (s *Server) handleGetJob(c echo.Context) error {
	task, err := s.Tasks.Get(c.Request().Context(), getTenant(c), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, task)
}

// handleCancelJob cancels a queued or running task.
func (s *Server) handleCancelJob(c echo.Context) error {
	id := c.Param("id")
	var err error
	if s.TaskWorker != nil {
		err = s.TaskWorker.CancelTask(c.Request().Context(), getTenant(c), id)
	} else {
		err = s.Tasks.Cancel(c.Request().Context(), getTenant(c), id)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) runEngineChat(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	message string,
	attachments []taskqueue.Attachment,
) (string, error) {
	stamp := execution.AgentExecutionStamp{ExecutionScope: execution.ScopeLegacyOrchestrator}
	if rec != nil {
		stamp.AgentID = rec.ID
		stamp.AgentVersion = rec.Version
	}
	return s.runEngineChatRecord(ctx, tenant, rec, stamp, message, attachments, true)
}

func (s *Server) runVersionedEngineChat(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	message string,
	attachments []taskqueue.Attachment,
) (string, error) {
	return s.runEngineChatRecord(ctx, tenant, rec, stamp, message, attachments, false)
}

func (s *Server) runEngineChatRecord(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	message string,
	attachments []taskqueue.Attachment,
	resolveCurrentSkills bool,
) (string, error) {
	resolved := rec
	if resolveCurrentSkills {
		copy := *rec
		if s.Store != nil {
			copy.Spec.Skills = compiler.ResolveSkillsFromStore(rec.Spec.Skills, s.Store, tenant)
		}
		resolved = &copy
	}
	execAttachments := make([]execenv.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		execAttachments = append(execAttachments, execenv.Attachment{
			Filename: attachment.Filename,
			Path:     attachment.Path,
		})
	}
	executor := s.engineExecutorFor(rec.RuntimeID != "")
	if executor == nil {
		return "", fmt.Errorf("engine executor is unavailable")
	}
	result, err := executor.ExecRemote(ctx, tenant, resolved, stamp, message, execAttachments)
	return result.Output, err
}

// ExecuteChat runs a chat request without HTTP context.
// Implements taskqueue.ChatExecutor for the asynchronous worker.
func (s *Server) ExecuteChat(ctx context.Context, tenant string, req taskqueue.ChatExecRequest) (*taskqueue.ChatExecResult, error) {
	requestFinalized := req.ClientRequestID == ""
	// A task reclaimed after its worker died re-enters ExecuteChat with the
	// same payload while the chat request is still 'running'. Capture that so
	// the user turn written by the previous attempt is not appended twice.
	resumedRunningRequest := false
	if !requestFinalized && s.ChatRequests != nil {
		if current, err := s.ChatRequests.Get(ctx, tenant, req.UserID, req.ClientRequestID); err == nil {
			resumedRunningRequest = current.Status == "running"
		}
		if _, err := s.ChatRequests.MarkRunning(ctx, tenant, req.UserID, req.ClientRequestID); err != nil {
			return nil, fmt.Errorf("mark chat request running: %w", err)
		}
		defer func() {
			if !requestFinalized {
				s.finishChatRequest(
					ctx, tenant, req.UserID, req.ClientRequestID, "failed", "",
					map[string]any{"project_id": req.ProjectID, "error": "execution_failed"},
				)
			}
		}()
	}
	// 1. Load agent spec.
	var (
		rec                 *registry.AgentRecord
		completionExecution *teamSessionExecution
		err                 error
	)
	completion := completionIdentity(req)
	if completion.CompletionID != "" {
		rec, completionExecution, err = s.acquireCompletionSessionEvent(ctx, tenant, completion)
		if err != nil {
			return nil, err
		}
		req.Agent = rec.Name
		req.ExecutionStamp = completionExecutionStamp(rec)
	} else {
		rec, err = resolveChatAgent(ctx, s.Registry, tenant, &req)
		if err != nil {
			return nil, err
		}
	}
	if s.metaTeamRunDisabled(rec.Name, "") {
		return nil, errMetaTeamDisabled
	}
	rec, err = s.applyTaskRuntimeAssignment(ctx, tenant, rec, req.RuntimeAssignment)
	if err != nil {
		return nil, fmt.Errorf("apply frozen runtime assignment: %w", err)
	}
	var teamExecution *teamSessionExecution
	if completionExecution == nil && req.RunSnapshotID != "" {
		teamExecution, err = s.loadAsyncTeamSessionExecution(ctx, tenant, rec, req)
		if err != nil {
			return nil, err
		}
	}
	sessionExecution := completionExecution
	if sessionExecution == nil {
		sessionExecution = teamExecution
	}
	ctx = contextWithTeamSessionExecution(ctx, sessionExecution)
	useNameResolver := chatUsesNameResolver(req.ExecutionStamp, req.NoDispatch)

	// 2. Resolve session + conversation. Ordinary async runs use a fresh or
	// caller-pinned session. Completion synthesis uses the room session selected
	// during lease admission but persists neither its driver prompt nor result
	// directly; the final outbox owns those projections.
	sessionID := req.SessionID
	if sessionExecution != nil {
		sessionID = sessionExecution.Lease.Key.SessionID
	} else if sessionID == "" {
		sessionID = uuid.New().String()
	}
	userID := req.UserID
	if sessionExecution != nil {
		userID = sessionExecution.Lease.Key.UserID
	}
	sessionKey := tenant + ":" + userID + ":" + req.Agent + ":" + sessionID
	if sessionExecution != nil {
		sessionKey = sessionExecution.LoomSessionKey
	}
	conversationID := req.ConversationID
	if sessionExecution != nil {
		conversationID = sessionExecution.ConversationID
	}
	if s.Conversations != nil {
		var conv conversation.Conversation
		switch {
		case completionExecution != nil:
			// Admission resolved the immutable room binding before acquiring the
			// new lease. The internal synthesis prompt is never persisted here.
		case conversationID == "":
			if req.ProjectID == "" {
				conv, err = s.Conversations.EnsureConversation(ctx, tenant, rec.ID, userID)
			} else {
				conv, err = s.Conversations.EnsureProjectConversationChannel(
					ctx, tenant, req.ProjectID, rec.ID, userID, "",
				)
			}
			if err != nil {
				return nil, err
			}
			conversationID = conv.ID
			if _, err := s.Conversations.BindSessionKey(ctx, tenant, conv.ID, sessionKey); err != nil {
				return nil, err
			}
			if _, err := s.Conversations.AppendMessage(ctx, conversation.Message{
				ConversationID: conv.ID,
				WorkspaceID:    tenant,
				Role:           "user",
				Content:        req.Message,
			}); err != nil {
				return nil, err
			}
		default:
			conv, err = s.Conversations.GetConversation(ctx, tenant, conversationID)
			if err != nil {
				return nil, err
			}
			if err := validateChatConversationParticipant(conv, tenant, rec.ID, userID); err != nil {
				return nil, err
			}
			if req.ProjectID != "" && conv.ProjectID != req.ProjectID {
				return nil, fmt.Errorf("conversation %q does not match chat project", conv.ID)
			}
			conversationID = conv.ID
			if _, err := s.Conversations.BindSessionKey(ctx, tenant, conv.ID, sessionKey); err != nil {
				return nil, err
			}
			// The user message was already recorded when the task was enqueued.
		}
	}

	var msgs []contract.Message
	msgs, _ = stdlib.LoadSession(s.Store, sessionKey)
	userMessage := contract.Message{Role: "user", Content: injectAttachmentNotice(req.Message, req.Attachments)}
	userTurnAlreadyPersisted := resumedRunningRequest &&
		len(msgs) > 0 && msgs[len(msgs)-1].Role == "user" &&
		msgs[len(msgs)-1].Content == userMessage.Content
	if !userTurnAlreadyPersisted {
		msgs = append(msgs, userMessage)
	}

	if completionExecution == nil {
		// Persist the user turn before running so a failed async run does not
		// drop the turn from session history for the next dispatch. Completion
		// synthesis prompts are intentionally never persisted here. Reclaimed
		// task retries skip the write because the previous attempt already
		// persisted this exact turn.
		if !userTurnAlreadyPersisted {
			_ = s.appendSessionMessages(ctx, sessionKey, userMessage)
		}

	}

	var output, stopReason, runID string
	var resultState loom.State
	var executionErr error
	var runtimeYielded bool
	if engine.IsCLIEngine(rec.Engine) && sessionExecution == nil {
		if req.ExecutionStamp != nil {
			output, err = s.runVersionedEngineChat(
				ctx, tenant, rec, *req.ExecutionStamp, req.Message, req.Attachments,
			)
		} else {
			output, err = s.runEngineChat(ctx, tenant, rec, req.Message, req.Attachments)
		}
		if err != nil {
			return nil, err
		}
		stopReason = "completed"
		if sessionExecution != nil {
			runID = sessionExecution.Lease.ActiveRunID
			resultState = loom.State{"output": output}
		}
	} else {
		// 3. Resolve the workspace LLM snapshot, then compile and run.
		llm, llmErr := s.llmFor(ctx, tenant)
		if llmErr != nil {
			return nil, llmErr
		}
		// NOTE: async paths intentionally do not set compileOpts.Embedder
		// (semantic skill matching falls back to keyword); unifying that with
		// the synchronous chat path is a separate work order.
		memSvc := s.memoryFor(ctx, tenant)
		tools := s.buildToolDispatcher(
			rec, tenant, userID, conversationID, llm, memSvc, !useNameResolver, ctx,
		)
		effort := contract.EffortLevel(req.Effort)
		if effort == "" {
			effort = contract.EffortMedium
		}

		compileContext := s.contextWithOwnerProfile(ctx, tenant, userID, rec, req.Context)
		compileOpts := compiler.CompileOpts{
			Profile: req.Profile,
			Context: compileContext,
			Effort:  effort,
			Store:   s.Store,
		}
		if useNameResolver {
			compileOpts.SubAgentStepResolver = s.resolveSubAgent
			compileOpts.AgentRunner = s.compilerAgentRunner(tenant, userID, llm, memSvc, nil)
		}
		if cfg := registry.EffectiveMemoryConfig(rec); memSvc != nil && cfg.Enabled {
			compileOpts.MemoryService = memSvc
			if cfg.TopK > 0 {
				compileOpts.MemoryTopK = cfg.TopK
			}
			compileOpts.AutoRemember = cfg.AutoRemember
			compileOpts.MemoryScope = cfg.Scope
		}
		configureProjectMemory(&compileOpts, tenant, rec, req.ProjectID)

		// Compile, run, extract — via the shared runner. Note: this also
		// closes an old divergence where the jobs path dropped "__profile"
		// from the input state; the runner sets the full contract key set.
		// LLM is the per-workspace snapshot resolved above, not a process global.
		runInput := loomruntime.Input{
			Messages:        msgs,
			LastUserMessage: req.Message,
			SessionID:       sessionID,
			UserID:          userID,
			Profile:         req.Profile,
			Context:         compileContext,
		}
		dependencies := loomruntime.Dependencies{
			LLM:                llm,
			Tools:              tools,
			Store:              s.Store,
			LifecycleHook:      s.RunLifecycleHook,
			SkillVersionReader: s.Skills,
			CompileOpts:        compileOpts,
		}
		if sessionExecution == nil && req.ExecutionStamp != nil &&
			req.ExecutionStamp.ExecutionScope != execution.ScopeLegacyOrchestrator {
			return nil, fmt.Errorf(
				"team-scoped job terminal attribution requires durable snapshot identity",
			)
		}
		var terminalAttribution loomruntime.TerminalAttribution
		if sessionExecution != nil {
			terminalAttribution = sessionExecution.TerminalAttribution
		} else {
			var attributionErr error
			terminalAttribution, attributionErr = legacyRootTerminalAttribution(tenant)
			if attributionErr != nil {
				return nil, attributionErr
			}
		}
		terminalSink, sinkErr := s.rootTerminalSink()
		if sinkErr != nil {
			return nil, sinkErr
		}
		dependencies.TerminalSink = terminalSink
		result, runErr := s.runChatSession(
			ctx, tenant, rec, runInput, terminalAttribution, dependencies,
			sessionExecution,
		)
		executionErr = runErr
		if runErr != nil && !result.Ran() && sessionExecution == nil {
			return nil, runErr
		}
		if runErr != nil {
			// Partial result: keep the old response semantics (use what ran)
			// but stop dropping the error silently.
			slog.Warn("graph.Run returned partial result", "agent", req.Agent, "error", runErr)
		}
		output = result.Output
		resultState = result.State
		stopReason = string(result.StopReason)
		runID = result.RunID
		runtimeYielded = result.Yielded
	}

	// 4. Persist the run. Completion synthesis closes its lease by committing
	// an outbox record; the delivery worker owns both user-visible projections.
	if completionExecution != nil {
		if executionErr != nil || runtimeYielded || strings.TrimSpace(output) == "" {
			failureErr := executionErr
			if failureErr == nil && runtimeYielded {
				failureErr = errors.New("completion synthesis cannot yield")
			}
			if failureErr == nil {
				failureErr = errTeamSessionBlankOutput
			}
			completionResult := loomruntime.Result{
				Output: output, StopReason: loom.StopReason(stopReason),
				RunID: runID, State: resultState, Yielded: runtimeYielded,
			}
			if err := s.finishTeamSessionExecution(
				ctx, completionExecution, completionResult, "", nil, failureErr,
			); err != nil {
				return nil, err
			}
			return nil, failureErr
		}
		metadataCtx := contextWithAssistantAgent(ctx, req.Agent)
		metadata := s.buildAssistantMetadata(metadataCtx, tenant, output)
		metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
		if err := s.finishCompletionSession(
			ctx,
			completionExecution,
			loomruntime.Result{
				Output: output, StopReason: loom.StopReason(stopReason),
				RunID: runID, State: resultState,
			},
			metadata,
			req.Context,
		); err != nil {
			return nil, err
		}
	} else if teamExecution != nil {
		metadataCtx := contextWithAssistantAgent(ctx, req.Agent)
		metadata := s.buildAssistantMetadata(metadataCtx, tenant, output)
		metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
		if err := s.finishTeamSessionExecution(
			ctx,
			teamExecution,
			loomruntime.Result{
				Output: output, StopReason: loom.StopReason(stopReason),
				RunID: runID, State: resultState,
				Yielded: runtimeYielded,
			},
			req.Message,
			metadata,
			executionErr,
		); err != nil {
			return nil, err
		}
	}
	switch {
	case sessionExecution != nil:
	case output != "":
		sessionMsgs := append(msgs, contract.Message{Role: "assistant", Content: output})
		_ = stdlib.SaveSession(s.Store, sessionKey, sessionMsgs)
	default:
		if resultMsgs, err := stdlib.GetMessages(resultState); err == nil && len(resultMsgs) > 0 {
			_ = stdlib.SaveSession(s.Store, sessionKey, resultMsgs)
		}
	}
	if sessionExecution == nil && output != "" && conversationID != "" {
		metadataCtx := contextWithAssistantAgent(ctx, req.Agent)
		metadata := s.buildAssistantMetadata(metadataCtx, tenant, output)
		metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
		_, err := s.Conversations.AppendMessage(ctx, conversation.Message{
			ConversationID: conversationID,
			WorkspaceID:    tenant,
			Role:           "assistant",
			Content:        output,
			Metadata:       metadata,
		})
		if err != nil {
			return nil, err
		}
	}
	if sessionExecution == nil {
		s.startOwnerMemoryFill(
			tenant, rec, userID, conversationID, req.Message, output, req.NoDispatch,
		)
	}

	result := &taskqueue.ChatExecResult{
		Output:            output,
		StopReason:        stopReason,
		SessionID:         sessionID,
		RunID:             runID,
		ProjectID:         req.ProjectID,
		ConversationID:    conversationID,
		RuntimeAssignment: req.RuntimeAssignment,
	}
	status := "completed"
	if result.StopReason == "yield" || result.StopReason == "yielded" {
		status = "yielded"
	}
	s.finishChatRequest(ctx, tenant, req.UserID, req.ClientRequestID, status, result.RunID, result)
	requestFinalized = true
	return result, nil
}

func taskRuntimeAssignmentsEqual(left, right *taskqueue.RuntimeAssignment) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func validateChatConversationParticipant(
	conv conversation.Conversation,
	workspaceID, agentID, userID string,
) error {
	if conv.WorkspaceID != workspaceID || conv.AgentID != agentID || conv.UserID != userID {
		return fmt.Errorf("conversation %q does not match chat participant", conv.ID)
	}
	return nil
}

// withTaskGroupProvenance appends a one-line plain-text provenance footer to a
// fan-out synthesis report delivered into a room session:
//
//	来源 · 任务组 {group_id} · 员工 {worker 名单} · run {run_id 短码}
//
// Every value comes from task-group truth the reconciler put on the exec
// request context (see internal/fanout/reconciler.go) and from this run's own
// ID — nothing is invented. Requests without task-group context (ordinary
// chat) pass through unchanged.
func withTaskGroupProvenance(output string, reqContext map[string]any, runID string) string {
	groupID, _ := reqContext["task_group_id"].(string)
	if groupID == "" {
		return output
	}
	workers := strings.Join(contextStringList(reqContext["task_group_workers"]), "、")
	if workers == "" {
		workers = "无"
	}
	return strings.TrimRight(output, "\n") + "\n\n" +
		fmt.Sprintf("来源 · 任务组 %s · 员工 %s · run %s", groupID, workers, shortRunID(runID))
}

// contextStringList tolerates the []any shape a []string takes after the exec
// request's JSON payload round-trip.
func contextStringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		list := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				list = append(list, text)
			}
		}
		return list
	default:
		return nil
	}
}

// shortRunID truncates a run ID to its leading 8 chars for display; an empty
// run ID (e.g. a CLI-engine run that never minted one) renders as "unknown".
func shortRunID(runID string) string {
	if runID == "" {
		return "unknown"
	}
	if len(runID) <= 8 {
		return runID
	}
	return runID[:8]
}
