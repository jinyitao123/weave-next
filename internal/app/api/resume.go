package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/sessionexec"
	"github.com/labstack/echo/v4"
)

// ResumeRequest is the input to the resume endpoint.
type ResumeRequest struct {
	RunID  string         `json:"run_id"`
	Agent  string         `json:"agent"`
	Input  map[string]any `json:"input"`
	Stream bool           `json:"stream"`
}

type apiRunCheckpoint struct {
	Schema int        `json:"schema_version"`
	RunID  string     `json:"run_id"`
	Graph  string     `json:"graph"`
	Seq    int64      `json:"seq"`
	State  loom.State `json:"state"`
}

func loadAPIResumeCheckpoint(
	ctx context.Context,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
) (apiRunCheckpoint, error) {
	if store == nil {
		return apiRunCheckpoint{}, fmt.Errorf("resume checkpoint store is unavailable")
	}
	graph := tenant + ":" + agent
	data, err := store.Get(ctx, "checkpoint:"+graph, runID)
	if err != nil {
		return apiRunCheckpoint{}, err
	}
	var checkpoint apiRunCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&checkpoint); err != nil {
		return apiRunCheckpoint{}, fmt.Errorf("decode resume checkpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return apiRunCheckpoint{}, fmt.Errorf("decode resume checkpoint: trailing JSON")
	}
	if checkpoint.RunID != runID || checkpoint.Graph != graph || checkpoint.State == nil {
		return apiRunCheckpoint{}, fmt.Errorf("resume checkpoint ownership mismatch")
	}
	return checkpoint, nil
}

func loadTeamAPIResumeCheckpoint(
	ctx context.Context,
	store loom.Store,
	snap snapshot.TeamRunSnapshot,
	runID string,
) (apiRunCheckpoint, error) {
	if store == nil {
		return apiRunCheckpoint{}, fmt.Errorf("resume checkpoint store is unavailable")
	}
	graph := teamCheckpointGraph(snap)
	data, err := store.Get(ctx, "checkpoint:"+graph, runID)
	if err != nil {
		return apiRunCheckpoint{}, err
	}
	var checkpoint apiRunCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&checkpoint); err != nil {
		return apiRunCheckpoint{}, fmt.Errorf("decode team resume checkpoint: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return apiRunCheckpoint{}, fmt.Errorf("decode team resume checkpoint: trailing JSON")
	}
	if checkpoint.RunID != runID || checkpoint.Graph != graph || checkpoint.State == nil {
		return apiRunCheckpoint{}, fmt.Errorf("team resume checkpoint ownership mismatch")
	}
	return checkpoint, nil
}

func teamCheckpointGraph(snap snapshot.TeamRunSnapshot) string {
	return "team:" + snap.WorkspaceID + ":" + snap.TeamID + ":" + snap.RunID
}

func (s *Server) resolveResumeAgentForEntry(
	ctx context.Context,
	tenant string,
	agent string,
	runID string,
) (*registry.AgentRecord, *execution.AgentExecutionStamp, error) {
	return resolveResumeAgent(ctx, s.Registry, s.Store, tenant, agent, runID)
}

func (s *Server) activateTeamSessionResume(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	runID string,
	kind sessionexec.YieldKind,
	input loom.State,
) (*teamSessionExecution, error) {
	if s.StoreExt == nil || s.Snapshots == nil || rec == nil {
		return nil, nil
	}
	snap, err := s.Snapshots.GetByRunID(ctx, tenant, runID)
	if errors.Is(err, snapshot.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if snap.Mode != "free_collab" || snap.LeadAvatarID == "" || snap.LeadAvatarID != rec.ID {
		return nil, nil
	}
	var checkpoint apiRunCheckpoint
	checkpoint, err = loadAPIResumeCheckpoint(ctx, s.Store, tenant, rec.Name, runID)
	if err != nil {
		return nil, err
	}
	userID, _ := checkpoint.State["user_id"].(string)
	sessionID, _ := checkpoint.State["session_id"].(string)
	if userID == "" || sessionID == "" {
		return nil, nil
	}
	key := sessionexec.SessionKey{
		WorkspaceID: tenant, UserID: userID,
		LeadAvatarID: snap.LeadAvatarID, SessionID: sessionID,
	}
	tx, err := s.StoreExt.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lease, err := sessionexec.NewPGStore().Get(ctx, tx, key)
	if errors.Is(err, sessionexec.ErrLeaseNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	token, _ := checkpoint.State["__yield_token"].(string)
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode team resume input: %w", err)
	}
	resumed, err := sessionexec.NewPGStore().ValidateAndActivate(
		ctx,
		tx,
		sessionexec.ResumeRequest{
			Key: key, LeaseEpoch: lease.LeaseEpoch, ActiveRunID: runID,
			YieldKind: kind, ResumeToken: token,
			YieldGeneration: lease.YieldGeneration, Input: inputJSON,
		},
	)
	if err != nil {
		return nil, err
	}
	conversationID := ""
	loomSessionKey := tenant + ":" + userID + ":" + rec.Name + ":" + sessionID
	_ = tx.QueryRow(ctx, `SELECT id FROM weave_conversations
		WHERE workspace_id=$1 AND session_key=$2 AND parent_message_id IS NULL
		ORDER BY created_at LIMIT 1`,
		tenant, loomSessionKey,
	).Scan(&conversationID)
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit team session resume activation: %w", err)
	}
	return &teamSessionExecution{
		Lease: resumed.Lease, Snapshot: *snap,
		AcquireEventID: fmt.Sprintf(
			"resume:%s:%d", runID, lease.YieldGeneration,
		),
		LoomSessionKey: loomSessionKey, ConversationID: conversationID,
		NextYieldGeneration: lease.YieldGeneration + 1,
		memoryOutboxReady:   make(chan struct{}),
	}, nil
}

func respondTeamSessionResumeError(c echo.Context, err error) error {
	status := http.StatusConflict
	payload := map[string]any{"error": err.Error()}
	var validation *sessionexec.ResumeInputValidationError
	if errors.As(err, &validation) {
		status = http.StatusBadRequest
		payload["code"] = sessionexec.ErrResumeInputInvalid.Error()
		payload["problems"] = validation.Problems
	} else {
		for _, sentinel := range []error{
			sessionexec.ErrLeaseNotFound,
			sessionexec.ErrLeaseEpochFenced,
			sessionexec.ErrLeaseRunMismatch,
			sessionexec.ErrLeaseStateConflict,
			sessionexec.ErrYieldKindMismatch,
			sessionexec.ErrResumeTokenInvalid,
			sessionexec.ErrYieldGenerationConflict,
			sessionexec.ErrLeaseExpired,
		} {
			if errors.Is(err, sentinel) {
				payload["code"] = sentinel.Error()
				break
			}
		}
	}
	return c.JSON(status, payload)
}

func checkpointResolutionHTTPStatus(err error) int {
	if errors.Is(err, loom.ErrCheckpointNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func resolveResumeAgent(
	ctx context.Context,
	reg chatAgentRegistry,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
) (*registry.AgentRecord, *execution.AgentExecutionStamp, error) {
	stamp, err := loomruntime.LoadRunAgentExecutionStamp(
		ctx, store, tenant, agent, runID,
	)
	if err != nil {
		return nil, nil, err
	}
	return resolveCheckpointAgentRecord(ctx, reg, tenant, agent, stamp)
}

func resolveCheckpointAgentRecord(
	ctx context.Context,
	reg chatAgentRegistry,
	tenant string,
	agent string,
	stamp *execution.AgentExecutionStamp,
) (*registry.AgentRecord, *execution.AgentExecutionStamp, error) {
	if stamp == nil {
		rec, err := reg.Get(ctx, tenant, agent)
		return rec, nil, err
	}
	rec, err := reg.GetVersion(ctx, tenant, stamp.AgentID, stamp.AgentVersion)
	if err != nil {
		return nil, stamp, err
	}
	if rec == nil || rec.ID != stamp.AgentID || rec.Version != stamp.AgentVersion ||
		rec.WorkspaceID != tenant || rec.Name == "" {
		return nil, stamp, fmt.Errorf(
			"frozen agent identity does not match %q@%d", stamp.AgentID, stamp.AgentVersion,
		)
	}
	if rec.Name != agent {
		return nil, stamp, fmt.Errorf(
			"frozen agent name %q does not match resume locator %q", rec.Name, agent,
		)
	}
	if stamp.ExecutionScope == execution.ScopeTeamWorkerLeaf {
		if err := registry.ValidateTeamWorkerAgentRecord(rec); err != nil {
			return nil, stamp, err
		}
	}
	return rec, stamp, nil
}

// resumeUsesNameResolver preserves the R7 legacy compatibility boundary:
// legacy orchestration may resolve declared sub-agents by name to latest,
// while leaf and free-collaboration scopes never receive a name resolver.
func resumeUsesNameResolver(stamp *execution.AgentExecutionStamp) bool {
	return stamp == nil || stamp.ExecutionScope == execution.ScopeLegacyOrchestrator
}

func bindResumeInputIdentity(
	input loom.State,
	tenant string,
	rec *registry.AgentRecord,
) loom.State {
	bound := make(loom.State, len(input)+2)
	for key, value := range input {
		bound[key] = value
	}
	bound["tenant"] = tenant
	bound["agent_name"] = rec.Name
	delete(bound, "__agent_id")
	delete(bound, "__agent_version")
	delete(bound, "__execution_scope")
	delete(bound, "__terminal_attribution")
	return bound
}

func (s *Server) handleResume(c echo.Context) error {
	var req ResumeRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.RunID == "" || req.Agent == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "run_id and agent are required"})
	}
	if !req.Stream {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "stream_required"})
	}

	tenant := getTenant(c)
	userID := getUserID(c)
	ctx := c.Request().Context()

	// Resolve the immutable agent identity from the checkpoint. The request
	// agent name only locates the graph namespace.
	rec, stamp, err := s.resolveResumeAgentForEntry(ctx, tenant, req.Agent, req.RunID)
	if err != nil {
		return c.JSON(checkpointResolutionHTTPStatus(err), map[string]string{"error": err.Error()})
	}

	// Merge human input into state.
	input := bindResumeInputIdentity(loom.State(req.Input), tenant, rec)
	input["user_id"] = userID
	teamExecution, err := s.activateTeamSessionResume(
		ctx, tenant, rec, req.RunID, sessionexec.YieldInput, loom.State(req.Input),
	)
	if err != nil {
		return respondTeamSessionResumeError(c, err)
	}
	rec, _, err = s.applySnapshotRuntimeAssignment(
		ctx, tenant, rec, teamExecution,
	)
	if err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "runtime_assignment_unavailable"})
	}
	llm, err := s.llmForLoomNode(ctx, tenant, rec, teamExecution)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	ctx = contextWithTeamSessionExecution(ctx, teamExecution)
	c.SetRequest(c.Request().WithContext(ctx))

	return s.handleResumeStream(
		c, tenant, userID, rec, stamp, req, input, llm, teamExecution,
	)
}

// handleResumeStream resumes a yielded graph with SSE streaming output.
func (s *Server) handleResumeStream(
	c echo.Context,
	tenant string,
	userID string,
	rec *registry.AgentRecord,
	stamp *execution.AgentExecutionStamp,
	req ResumeRequest,
	input loom.State,
	llm contract.LLM,
	teamExecution *teamSessionExecution,
) error {

	sse, err := NewSSEWriter(c)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Build read-only tool set for UI hints.
	memSvc := s.memoryFor(c.Request().Context(), tenant)
	useNameResolver := resumeUsesNameResolver(stamp)
	tools := s.buildToolDispatcher(rec, tenant, userID, "", llm, memSvc, !useNameResolver, c.Request().Context())
	roTools := map[string]bool{}
	if defs, err := tools.ListTools(c.Request().Context()); err == nil {
		for _, d := range defs {
			if d.ReadOnly {
				roTools[d.Name] = true
			}
		}
	}
	streamLLM := &StreamingLLMAdapter{inner: llm, sse: sse, readOnlyTools: roTools}

	// Collect tool calls for session persistence.
	var toolCallRecords []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	var tcMu sync.Mutex

	toolResultHook := contract.ToolHook{
		Post: func(_ context.Context, call contract.ToolCall, result *contract.ToolResult) error {
			status := "success"
			if result.IsError {
				status = "error"
			}
			_ = sse.SendEvent("tool_result", map[string]any{
				"name":    call.Name,
				"content": result.Content,
				"status":  status,
			})
			tcMu.Lock()
			toolCallRecords = append(toolCallRecords, struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			}{Name: call.Name, Status: status})
			tcMu.Unlock()
			return nil
		},
	}

	compileOpts := compiler.CompileOpts{
		Store:           s.Store,
		ToolHooks:       []contract.ToolHook{toolResultHook},
		BeforeStepHooks: []loom.StepHook{SSEStepStartHook()},
		AfterStepHooks:  []loom.StepHook{SSEStepEndHook()},
	}
	if useNameResolver {
		compileOpts.SubAgentStepResolver = s.resolveSubAgent
		compileOpts.AgentRunner = s.compilerAgentRunner(tenant, userID, llm, memSvc, nil)
	}
	if cfg := registry.EffectiveMemoryConfig(rec); memSvc != nil && cfg.Enabled {
		compileOpts.MemoryService = memSvc
		compileOpts.MemoryTopK = cfg.TopK
		compileOpts.MemoryScope = cfg.Scope
		compileOpts.AutoRemember = cfg.AutoRemember
		compileOpts.HookLLM = llm
	}
	projectID, projectErr := s.projectMemoryIDForRun(
		c.Request().Context(), tenant, req.RunID, teamExecution,
	)
	if projectErr != nil {
		_ = sse.SendEvent("done", map[string]any{"error": "project_memory_attribution_failed"})
		return nil
	}
	configureProjectMemory(&compileOpts, tenant, rec, projectID)
	ctx := ContextWithSSE(c.Request().Context(), sse)
	ctx = contextWithTeamSessionExecution(ctx, teamExecution)
	terminalSink, sinkErr := s.rootTerminalSink()
	if sinkErr != nil {
		_ = sse.SendEvent("done", map[string]any{"error": sinkErr.Error()})
		return nil
	}
	dependencies := loomruntime.Dependencies{
		LLM:                streamLLM,
		Tools:              tools,
		Store:              s.Store,
		TerminalSink:       terminalSink,
		LifecycleHook:      s.RunLifecycleHook,
		SkillVersionReader: s.Skills,
		CompileOpts:        compileOpts,
	}
	result, runErr := loomruntime.Resume(ctx, tenant, rec, req.RunID, input, dependencies)
	if teamExecution != nil {
		resumeInput, _ := json.Marshal(req.Input)
		metadataCtx := contextWithAssistantAgent(ctx, rec.Name)
		metadata := s.buildAssistantMetadata(metadataCtx, tenant, result.Output)
		metadata = mergeRuntimeAssignmentMetadata(metadata, runtimeAssignmentForTeamExecution(teamExecution))
		if err := s.finishTeamSessionExecution(
			ctx, teamExecution, result, string(resumeInput), metadata, runErr,
		); err != nil {
			_ = sse.SendEvent("done", map[string]any{
				"run_id": req.RunID,
				"error":  err.Error(),
			})
			return nil
		}
	}

	// Emit yield or done event.
	if result.Yielded {
		yieldData := map[string]any{
			"run_id":             result.RunID,
			"prompt":             "Agent is waiting for your input.",
			"runtime_assignment": runtimeAssignmentForTeamExecution(teamExecution),
		}
		appendTeamExecutionEventIdentity(yieldData, teamExecution)
		// Forward yield_type from state if present.
		if yt, ok := result.State["yield_type"].(string); ok {
			yieldData["yield_type"] = yt
		}
		_ = sse.SendEvent("yield", yieldData)
		return nil
	}

	doneData := map[string]any{
		"runtime_assignment": runtimeAssignmentForTeamExecution(teamExecution),
	}
	appendTeamExecutionEventIdentity(doneData, teamExecution)
	if result.Ran() {
		doneData["output"] = result.Output
		doneData["stop_reason"] = string(result.StopReason)
		doneData["usage"] = result.Usage
		doneData["run_id"] = result.RunID
	}
	if runErr != nil {
		doneData["error"] = runErr.Error()
	}
	_ = sse.SendEvent("done", doneData)

	return nil
}

func runtimeAssignmentForTeamExecution(execution *teamSessionExecution) *runtimes.Assignment {
	if execution == nil {
		return nil
	}
	assignment, _ := snapshotRuntimeAssignment(execution.Snapshot)
	return assignment
}

func appendTeamExecutionResponseIdentity(response *ChatResponse, execution *teamSessionExecution) {
	if response == nil || execution == nil {
		return
	}
	response.ProjectID = execution.Lease.ProjectID
	response.ConversationID = execution.ConversationID
	response.SessionID = execution.Lease.Key.SessionID
}

func appendTeamExecutionEventIdentity(payload map[string]any, execution *teamSessionExecution) {
	if execution == nil {
		return
	}
	payload["project_id"] = execution.Lease.ProjectID
	payload["conversation_id"] = execution.ConversationID
	payload["session_id"] = execution.Lease.Key.SessionID
}
