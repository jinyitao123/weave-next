package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/labstack/echo/v4"
)

// ForkRunRequest selects an immutable historical checkpoint to resume as a
// new run.
type ForkRunRequest struct {
	Seq    int64          `json:"seq"`
	Agent  string         `json:"agent"`
	Input  map[string]any `json:"input,omitempty"`
	Stream bool           `json:"stream,omitempty"`
}

// ForkRunResponse identifies a new run and its direct source checkpoint.
type ForkRunResponse struct {
	RunID       string `json:"run_id"`
	ParentRunID string `json:"parent_run_id"`
	ParentSeq   int64  `json:"parent_seq"`
}

func resolveForkAgent(
	ctx context.Context,
	reg chatAgentRegistry,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
	seq int64,
) (*registry.AgentRecord, *execution.AgentExecutionStamp, error) {
	stamp, err := loomruntime.LoadRunAgentExecutionStampAt(
		ctx, store, tenant, agent, runID, seq,
	)
	if err != nil {
		return nil, nil, err
	}
	return resolveCheckpointAgentRecord(ctx, reg, tenant, agent, stamp)
}

func (s *Server) handleForkRun(c echo.Context) error {
	var req ForkRunRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid request body"})
	}
	if req.Agent == "" || req.Seq <= 0 {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "agent and a positive seq are required"})
	}
	if !req.Stream {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "stream_required"})
	}

	tenant := getTenant(c)
	userID := getUserID(c)
	ctx := c.Request().Context()
	parentRunID := c.Param("id")
	if s.Registry == nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "agent registry is unavailable"})
	}

	var rec *registry.AgentRecord
	var stamp *execution.AgentExecutionStamp
	var teamExecution *teamSessionExecution
	var err error
	if !teamAssemblerDisabled() && s.Snapshots != nil {
		if snap, snapErr := s.Snapshots.GetByRunID(ctx, tenant, parentRunID); snapErr == nil && snap.Mode == "free_collab" {
			rec, stamp, err = s.resolveResumeAgentForEntry(ctx, tenant, req.Agent, parentRunID)
			if err == nil {
				teamExecution = &teamSessionExecution{Snapshot: *snap}
				ctx = contextWithTeamSessionExecution(ctx, teamExecution)
			}
		} else if snapErr != nil && !errors.Is(snapErr, snapshot.ErrNotFound) {
			err = snapErr
		}
	}
	if rec == nil && err == nil {
		rec, stamp, err = resolveForkAgent(
			ctx, s.Registry, s.Store, tenant, req.Agent, parentRunID, req.Seq,
		)
	}
	if err != nil {
		return c.JSON(checkpointResolutionHTTPStatus(err), map[string]string{"error": err.Error()})
	}
	rec, _, err = s.applySnapshotRuntimeAssignment(ctx, tenant, rec, teamExecution)
	if err != nil {
		return c.JSON(http.StatusConflict, map[string]string{"error": "runtime_assignment_unavailable"})
	}

	sse, err := NewSSEWriter(c)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	ctx = ContextWithSSE(ctx, sse)
	c.SetRequest(c.Request().WithContext(ctx))
	terminalError := func(runID string, err error) error {
		_ = sse.SendEvent("done", map[string]any{
			"run_id": runID, "parent_run_id": parentRunID, "parent_seq": req.Seq, "error": err.Error(),
		})
		return nil
	}
	llm, err := s.llmForLoomNode(ctx, tenant, rec, teamExecution)
	if err != nil {
		return terminalError("", err)
	}

	input := make(loom.State, len(req.Input)+1)
	for key, value := range req.Input {
		input[key] = value
	}
	input["user_id"] = userID

	memSvc := s.memoryFor(ctx, tenant)
	useNameResolver := resumeUsesNameResolver(stamp)
	tools := s.buildToolDispatcher(rec, tenant, userID, "", llm, memSvc, !useNameResolver, ctx)
	compileOpts := compiler.CompileOpts{Store: s.Store}
	roTools := map[string]bool{}
	if defs, listErr := tools.ListTools(ctx); listErr == nil {
		for _, definition := range defs {
			if definition.ReadOnly {
				roTools[definition.Name] = true
			}
		}
	}
	runLLM := &StreamingLLMAdapter{inner: llm, sse: sse, agentName: rec.Name, readOnlyTools: roTools}
	toolResultHook := contract.ToolHook{
		Post: func(_ context.Context, call contract.ToolCall, result *contract.ToolResult) error {
			status := "success"
			if result.IsError {
				status = "error"
			}
			_ = sse.SendEvent("tool_result", map[string]any{
				"name": call.Name, "content": result.Content, "status": status,
			})
			return nil
		},
	}
	compileOpts.ToolHooks = []contract.ToolHook{toolResultHook}
	compileOpts.BeforeStepHooks = []loom.StepHook{SSEStepStartHook()}
	compileOpts.AfterStepHooks = []loom.StepHook{SSEStepEndHook()}
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
	projectID, projectErr := s.projectMemoryIDForRun(ctx, tenant, parentRunID, teamExecution)
	if projectErr != nil {
		return terminalError("", errors.New("project_memory_attribution_failed"))
	}
	configureProjectMemory(&compileOpts, tenant, rec, projectID)
	terminalSink, err := s.rootTerminalSink()
	if err != nil {
		return terminalError("", err)
	}
	dependencies := loomruntime.Dependencies{
		LLM: runLLM, Tools: tools, Store: s.Store, TerminalSink: terminalSink,
		LifecycleHook: s.RunLifecycleHook, SkillVersionReader: s.Skills, CompileOpts: compileOpts,
	}
	if teamExecution != nil {
		dependencies.CompileAgent = s.teamCompileFactory(ctx, teamExecution, true, userID, memSvc)
	}
	request := loomruntime.RunRequest{
		Tenant: tenant, Agent: rec, Stamp: stamp, Dependencies: dependencies,
	}
	if teamExecution != nil {
		attribution, attributionErr := terminalAttributionFromSnapshot(tenant, teamExecution.Snapshot, nil)
		if attributionErr != nil {
			return terminalError("", attributionErr)
		}
		request.TerminalAttribution = &attribution
	}
	prepared, err := loomruntime.Prepare(request)
	if err != nil {
		return terminalError("", errors.New("agent compilation failed: "+err.Error()))
	}

	result, runErr := prepared.ResumeAt(ctx, parentRunID, req.Seq, input)
	if errors.Is(runErr, loom.ErrCheckpointNotFound) {
		return terminalError(result.RunID, errors.New("run checkpoint not found"))
	}
	if runErr != nil {
		terminal := map[string]any{
			"run_id": result.RunID, "parent_run_id": parentRunID, "parent_seq": req.Seq, "error": runErr.Error(),
		}
		if result.Ran() {
			terminal["output"] = result.Output
			terminal["stop_reason"] = string(result.StopReason)
		}
		_ = sse.SendEvent("done", terminal)
		return nil
	}
	terminalRunError := func(err error) error {
		terminal := map[string]any{
			"run_id": result.RunID, "parent_run_id": parentRunID, "parent_seq": req.Seq, "error": err.Error(),
		}
		if result.Ran() {
			terminal["output"] = result.Output
			terminal["stop_reason"] = string(result.StopReason)
		}
		_ = sse.SendEvent("done", terminal)
		return nil
	}

	history, historyErr := prepared.History(ctx, result.RunID)
	if historyErr != nil {
		return terminalRunError(errors.New("failed to inspect fork checkpoints"))
	}
	if len(history) == 0 && result.StopReason == loom.StopCompleted {
		return terminalRunError(errors.New("终端步骤不可分叉"))
	}
	terminal := map[string]any{
		"run_id": result.RunID, "parent_run_id": parentRunID, "parent_seq": req.Seq,
	}
	if result.Ran() {
		terminal["output"] = result.Output
		terminal["stop_reason"] = string(result.StopReason)
	}
	if result.Yielded {
		yieldType, _ := result.State["yield_type"].(string)
		terminal["yield_type"] = yieldType
		_ = sse.SendEvent("yield", terminal)
		return nil
	}
	_ = sse.SendEvent("done", terminal)
	return nil
}
