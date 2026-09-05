package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/conversation"
	"github.com/jinyitao123/weave/internal/base/streamctx"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/labstack/echo/v4"
)

// SSEWriter sends Server-Sent Events to the HTTP response.
type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

// NewSSEWriter creates a new SSE writer from an Echo context.
func NewSSEWriter(c echo.Context) (*SSEWriter, error) {
	w := c.Response().Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	return &SSEWriter{w: w, flusher: flusher}, nil
}

// ContextSuppressStream returns a context that tells StreamingLLMAdapter
// to bypass streaming and call the inner LLM directly.
func ContextSuppressStream(ctx context.Context) context.Context {
	return streamctx.SuppressStream(ctx)
}

func isSuppressStream(ctx context.Context) bool {
	return streamctx.IsSuppressStream(ctx)
}

// ContextWithSSE returns a new context carrying the SSEWriter.
func ContextWithSSE(ctx context.Context, sse *SSEWriter) context.Context {
	return streamctx.WithEventSender(ctx, sse)
}

// SSEFromContext retrieves the SSEWriter from context, or nil.
func SSEFromContext(ctx context.Context) *SSEWriter {
	sse, _ := streamctx.EventSenderFromContext(ctx).(*SSEWriter)
	return sse
}

// SendEvent sends a typed SSE event to the client.
func (s *SSEWriter) SendEvent(eventType string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, jsonData)
	if err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// SendComment sends an SSE comment frame, typically used as a keepalive.
func (s *SSEWriter) SendComment(comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := fmt.Fprintf(s.w, ": %s\n\n", comment); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// ---------------------------------------------------------------------------
// Step SSE hooks — emit step_start / step_end events to the streaming client
// ---------------------------------------------------------------------------

// SSEStepStartHook returns a Before hook that emits a "step_start" SSE event.
func SSEStepStartHook() loom.StepHook {
	return func(ctx context.Context, step string, _ loom.State) error {
		if sse := SSEFromContext(ctx); sse != nil {
			data := map[string]string{"step": step}
			if agent := executionAgentFromContext(ctx, ""); agent != "" {
				data["agent"] = agent
			}
			if err := sse.SendEvent("step_start", data); err != nil {
				return fmt.Errorf("SSE step_start send failed: %w", err)
			}
		}
		return nil
	}
}

// SSEStepEndHook returns an After hook that emits a "step_end" SSE event
// with step name and token usage (if available).
func SSEStepEndHook() loom.StepHook {
	return func(ctx context.Context, step string, state loom.State) error {
		if sse := SSEFromContext(ctx); sse != nil {
			data := map[string]any{"step": step}
			if agent := executionAgentFromContext(ctx, ""); agent != "" {
				data["agent"] = agent
			}
			if u, ok := state["usage"].(contract.Usage); ok {
				data["tokens_in"] = u.InputTokens
				data["tokens_out"] = u.OutputTokens
			}
			if err := sse.SendEvent("step_end", data); err != nil {
				return fmt.Errorf("SSE step_end send failed: %w", err)
			}
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// BlockStreamFilter — intercepts structured fenced blocks during streaming
// ---------------------------------------------------------------------------

// BlockStreamFilter sits between the LLM stream and SSE output.
// Normal text is forwarded as "chunk" events immediately.
// Structured fences (```chart, ```mermaid, ```svg) are buffered silently
// and emitted as "content_block" events when the fence closes.
type BlockStreamFilter struct {
	sse        *SSEWriter
	agentName  string
	buf        strings.Builder // full accumulated text
	sent       int             // bytes of buf content already emitted
	SentBlocks bool            // true if any structured blocks were handled
}

func (f *BlockStreamFilter) sendChunk(content string) {
	data := map[string]string{"content": content}
	if f.agentName != "" {
		data["agent"] = f.agentName
	}
	_ = f.sse.SendEvent("chunk", data)
}

// Feed adds a new chunk of text from the LLM stream.
func (f *BlockStreamFilter) Feed(chunk string) {
	f.buf.WriteString(chunk)
	f.process()
}

// Flush emits any remaining buffered content as chunk events.
func (f *BlockStreamFilter) Flush() {
	text := f.buf.String()
	remaining := text[f.sent:]
	if remaining != "" {
		f.sendChunk(remaining)
		f.sent = len(text)
	}
}

// FullContent returns the complete accumulated text (for ChatResponse).
func (f *BlockStreamFilter) FullContent() string {
	return f.buf.String()
}

// structuredLangs are fence languages that get intercepted as content_blocks.
var structuredLangs = map[string]bool{
	"chart": true, "mermaid": true, "svg": true, "component": true,
}

func (f *BlockStreamFilter) process() {
	text := f.buf.String()
	unsent := text[f.sent:]

	for len(unsent) > 0 {
		// Look for ``` in unsent text.
		idx := strings.Index(unsent, "```")
		if idx == -1 {
			safe := len(unsent)
			if strings.HasSuffix(unsent, "``") {
				safe = len(unsent) - 2
			} else if strings.HasSuffix(unsent, "`") {
				safe = len(unsent) - 1
			}
			if safe > 0 {
				f.sendChunk(unsent[:safe])
				f.sent += safe
			}
			return
		}

		afterFence := unsent[idx+3:]
		nlIdx := strings.Index(afterFence, "\n")
		if nlIdx == -1 {
			if idx > 0 {
				f.sendChunk(unsent[:idx])
				f.sent += idx
			}
			return
		}

		lang := strings.TrimSpace(strings.ToLower(afterFence[:nlIdx]))
		isStructured := structuredLangs[lang]
		bodyStart := idx + 3 + nlIdx + 1

		closeIdx := strings.Index(unsent[bodyStart:], "```")

		if closeIdx == -1 {
			if isStructured {
				if idx > 0 {
					f.sendChunk(unsent[:idx])
					f.sent += idx
				}
				return
			}
			f.sendChunk(unsent)
			f.sent += len(unsent)
			return
		}

		fenceEnd := bodyStart + closeIdx + 3

		if isStructured {
			if idx > 0 {
				f.sendChunk(unsent[:idx])
			}
			body := strings.TrimSpace(unsent[bodyStart : bodyStart+closeIdx])
			block := buildContentBlock(lang, body)
			if block != nil {
				f.SentBlocks = true
				filtered := filterContentBlocks([]ContentBlock{*block}, f.agentName)
				if len(filtered) > 0 {
					_ = f.sse.SendEvent("content_block", filtered[0])
				}
			} else {
				f.sendChunk(unsent[idx:fenceEnd])
			}
		} else {
			f.sendChunk(unsent[:fenceEnd])
		}

		f.sent += fenceEnd
		unsent = text[f.sent:]
	}
}

// buildContentBlock creates a typed ContentBlock from a fence language and body.
func buildContentBlock(lang, body string) *ContentBlock {
	switch lang {
	case "chart":
		blocks := appendChartBlock(nil, body)
		if len(blocks) > 0 {
			return &blocks[0]
		}
		return nil
	case "mermaid":
		return &ContentBlock{Type: "diagram", Format: "mermaid", Source: body}
	case "svg":
		return &ContentBlock{Type: "diagram", Format: "svg", Source: body}
	case "component":
		return buildComponentBlock(body)
	}
	return nil
}

// ---------------------------------------------------------------------------
// StreamingLLMAdapter — uses BlockStreamFilter for chunk interception
// ---------------------------------------------------------------------------

type StreamingLLMAdapter struct {
	inner         contract.LLM
	sse           *SSEWriter
	agentName     string
	SentBlocks    bool
	readOnlyTools map[string]bool // tool names that are read-only
	execution     *assistantExecutionRecorder
}

func (a *StreamingLLMAdapter) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	// Internal steps (e.g. intake_check) suppress streaming to avoid leaking JSON to UI.
	if streamctx.IsSuppressStream(ctx) {
		return a.inner.Chat(ctx, req)
	}

	ch, err := a.inner.Stream(ctx, req)
	if err != nil {
		return a.fallbackChat(ctx, req)
	}
	if ch == nil {
		return a.fallbackChat(ctx, req)
	}

	agentName := executionAgentFromContext(ctx, a.agentName)
	filter := &BlockStreamFilter{sse: a.sse, agentName: agentName}
	var usage contract.Usage
	var toolCalls []contract.ToolCall
	sawDone := false

	for chunk := range ch {
		// 协议错误（stream 错误传播通道）：fail-closed。错误块不带 Done——
		// 绝不能把错误当成干净完成，也不该转成第二次 blocking 调用掩盖它；
		// 空响应等可重试分类由上游 fallback 包装按 errors.Is 决定重试/切模型。
		if chunk.Err != nil {
			return nil, chunk.Err
		}
		if chunk.Content != "" {
			a.execution.text(ctx, a.agentName, chunk.Content)
			filter.Feed(chunk.Content)
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.ToolCalls...)
			for _, tc := range chunk.ToolCalls {
				a.execution.toolStarted(ctx, a.agentName, tc)
				tcEvent := map[string]any{"name": tc.Name, "args": tc.Args, "call_id": tc.ID, "agent": agentName}
				if a.readOnlyTools[tc.Name] {
					tcEvent["read_only"] = true
				}
				_ = a.sse.SendEvent("tool_call", tcEvent)
			}
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if chunk.Done {
			sawDone = true
		}
	}
	if !sawDone {
		return a.fallbackChat(ctx, req)
	}

	filter.Flush()
	if filter.SentBlocks {
		a.SentBlocks = true
	}

	return &contract.ChatResponse{
		Content:   filter.FullContent(),
		ToolCalls: toolCalls,
		Usage:     usage,
	}, nil
}

func (a *StreamingLLMAdapter) fallbackChat(
	ctx context.Context,
	req contract.ChatRequest,
) (*contract.ChatResponse, error) {
	if err := loomruntime.RestartUsageAttempt(ctx); err != nil {
		return nil, fmt.Errorf("restart usage attempt before Chat fallback: %w", err)
	}
	return a.inner.Chat(ctx, req)
}

func (a *StreamingLLMAdapter) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return a.inner.Stream(ctx, req)
}

// ---------------------------------------------------------------------------
// handleChatStream
// ---------------------------------------------------------------------------

func (s *Server) handleChatStream(c echo.Context, tenant string, rec *registry.AgentRecord,
	msgs []contract.Message, req ChatRequest, sessionKey, conversationID, userMessageID string,
	tools contract.ToolDispatcher, agentTool *mcphost.AgentToolDispatcher, fanoutTool *FanoutToolDispatcher,
	effort contract.EffortLevel, llm contract.LLM,
	teamExecution *teamSessionExecution,
) error {
	requestCtx := c.Request().Context()
	runCtx := context.WithoutCancel(requestCtx)
	// Extract user_id from the session key (passed through handleChat).
	userID := ""
	if parts := strings.SplitN(sessionKey, ":", 4); len(parts) >= 2 {
		userID = parts[1]
	}
	requestFinalized := req.ClientRequestID == ""
	defer func() {
		if !requestFinalized {
			s.finishChatRequest(
				runCtx, tenant, userID, req.ClientRequestID,
				"failed", "", map[string]any{
					"project_id": req.ProjectID, "conversation_id": conversationID,
					"user_message_id": userMessageID, "error": "stream_failed",
				},
			)
		}
	}()

	sse, err := NewSSEWriter(c)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	sendTerminalError := func(runID string, terminalErr error) error {
		doneData := map[string]any{
			"session_id":         req.SessionID,
			"project_id":         req.ProjectID,
			"conversation_id":    conversationID,
			"user_message_id":    userMessageID,
			"runtime_assignment": req.RuntimeAssignment,
			"error":              terminalErr.Error(),
		}
		if runID != "" {
			doneData["run_id"] = runID
		}
		s.finishChatRequest(
			runCtx, tenant, userID, req.ClientRequestID,
			"failed", runID, doneData,
		)
		requestFinalized = true
		_ = sse.SendEvent("done", doneData)
		return nil
	}

	// Build read-only tool set for UI hints.
	roTools := map[string]bool{}
	if defs, err := tools.ListTools(runCtx); err == nil {
		for _, d := range defs {
			if d.ReadOnly {
				roTools[d.Name] = true
			}
		}
	}
	executionRecorder := newAssistantExecutionRecorder(rec.Name, req.Message, time.Now())
	streamLLM := &StreamingLLMAdapter{inner: llm, sse: sse, agentName: rec.Name, readOnlyTools: roTools, execution: executionRecorder}

	// Collect tool calls for session persistence.
	type toolCallRecord struct {
		Name   string `json:"name"`
		Args   string `json:"args,omitempty"`
		Result string `json:"result,omitempty"`
		Status string `json:"status"`
	}
	var toolCallRecords []toolCallRecord
	var tcMu sync.Mutex

	toolResultHook := contract.ToolHook{
		Post: func(toolCtx context.Context, call contract.ToolCall, result *contract.ToolResult) error {
			status := "success"
			if result.IsError {
				status = "error"
			}
			_ = sse.SendEvent("tool_result", map[string]any{
				"call_id": call.ID,
				"name":    call.Name,
				"content": result.Content,
				"status":  status,
				"agent":   executionAgentFromContext(toolCtx, rec.Name),
			})
			executionRecorder.toolFinished(toolCtx, rec.Name, call, result)
			// Truncate result for persistence to avoid bloating session storage.
			persisted := result.Content
			if len(persisted) > 2000 {
				persisted = persisted[:2000] + "...(truncated)"
			}
			tcMu.Lock()
			toolCallRecords = append(toolCallRecords, toolCallRecord{
				Name:   call.Name,
				Args:   call.Args,
				Result: persisted,
				Status: status,
			})
			tcMu.Unlock()
			return nil
		},
	}

	compileContext := s.contextWithOwnerProfile(runCtx, tenant, userID, rec, req.Context)
	memSvc := s.memoryFor(runCtx, tenant)
	compileOpts := compiler.CompileOpts{
		Profile:              req.Profile,
		Context:              compileContext,
		Effort:               effort,
		ToolHooks:            []contract.ToolHook{toolResultHook},
		BeforeStepHooks:      []loom.StepHook{SSEStepStartHook()},
		AfterStepHooks:       []loom.StepHook{SSEStepEndHook()},
		Store:                s.Store,
		SubAgentStepResolver: s.resolveSubAgent,
		AgentRunner:          s.compilerAgentRunner(tenant, userID, llm, memSvc, nil),
	}
	// Enable memory + semantic skill matching if embedder is available.
	if memSvc != nil {
		compileOpts.Embedder = memSvc.Embedder()
		if cfg := registry.EffectiveMemoryConfig(rec); cfg.Enabled {
			compileOpts.MemoryService = memSvc
			if cfg.TopK > 0 {
				compileOpts.MemoryTopK = cfg.TopK
			}
			compileOpts.AutoRemember = cfg.AutoRemember
			compileOpts.MemoryScope = cfg.Scope
			compileOpts.HookLLM = llm
		}
	}
	configureProjectMemory(&compileOpts, tenant, rec, req.ProjectID)
	ctx := ContextWithSSE(runCtx, sse)
	ctx = contextWithExecutionAgent(ctx, rec.Name)
	ctx = contextWithTeamSessionExecution(ctx, teamExecution)
	var terminalAttribution loomruntime.TerminalAttribution
	if teamExecution != nil {
		terminalAttribution = teamExecution.TerminalAttribution
	} else {
		terminalAttribution, err = legacyRootTerminalAttribution(tenant)
		if err != nil {
			return sendTerminalError("", err)
		}
	}
	terminalSink, sinkErr := s.rootTerminalSink()
	if sinkErr != nil {
		return sendTerminalError("", sinkErr)
	}
	result, runErr := s.runChatSession(ctx, tenant, rec, loomruntime.Input{
		Messages:        msgs,
		LastUserMessage: req.Message,
		SessionID:       req.SessionID,
		UserID:          userID,
		Profile:         req.Profile,
		Context:         compileContext,
	}, terminalAttribution, loomruntime.Dependencies{
		LLM:                streamLLM,
		Tools:              tools,
		Store:              s.Store,
		TerminalSink:       terminalSink,
		LifecycleHook:      s.RunLifecycleHook,
		SkillVersionReader: s.Skills,
		CompileOpts:        compileOpts,
	}, teamExecution)
	executionRecorder.finish(result, runErr)
	var terminalOutcome *TerminalOutcome
	templateDraftReady := false
	if runErr != nil && !result.Ran() && teamExecution == nil {
		return sendTerminalError(result.RunID, runErr)
	}
	if teamExecution != nil {
		userContent := req.Message
		if n := len(msgs); n > 0 && msgs[n-1].Role == "user" {
			userContent = msgs[n-1].Content
		}
		var assistantFields map[string]any
		// For create_team discovery sessions, validate and render the output
		// before it is committed to the session store and sent in the done
		// event. Raw model text that is not a valid discovery protocol
		// output is never persisted as user-visible content.
		if result.Ran() && runErr == nil && s.Conversations != nil && conversationID != "" {
			if intent, intentErr := getConversationIntent(runCtx, s.Conversations, tenant, conversationID); intentErr == nil && intent == "create_team" {
				if _, ready := executionRecorder.lastSuccessfulToolResult("tf_render_template_draft"); ready {
					templateDraftReady = true
					result.Output = "团队模板草稿已生成。请在下方卡片中审阅 team.yaml，确认后即可创建待评测团队。"
				} else {
					var boundBuildRunID string
					var boundBuildRun *teambuild.TeamBuildRun
					var revisionToken *teambuild.BlueprintRevisionToken
					var transitionReason string
					if s.TeamBuild != nil {
						if run, runErr := s.TeamBuild.GetLatestBuildRunByConversation(runCtx, tenant, conversationID); runErr == nil {
							boundBuildRunID = run.BuildRunID
							boundBuildRun = &run
							transitionReason, _ = s.TeamBuild.GetLatestBuildRunTransitionReason(
								runCtx, tenant, run.BuildRunID,
							)
							if token, ok, tokenErr := s.currentBlueprintRevisionToken(runCtx, tenant, run.BuildRunID); tokenErr == nil && ok {
								revisionToken = token
							}
							if run.Status == teambuild.StatusPlanning && executionRecorder.lastToolCallFailed(
								teamBlueprintPlanToolName, teamDeclarativeWorkflowPlanToolName,
							) {
								transitionCtx, cancelTransition := context.WithTimeout(runCtx, 5*time.Second)
								if blocked, transitionErr := s.TeamBuild.TransitionStatus(
									transitionCtx, tenant, run.BuildRunID,
									teambuild.StatusPlanning, teambuild.StatusBlocked,
									req.Agent, reasonBlueprintValidationFailed,
								); transitionErr == nil {
									boundBuildRun = &blocked
									transitionReason = reasonBlueprintValidationFailed
									revisionToken = nil
								}
								cancelTransition()
							}
						}
					}
					discoveryOutput := extractDiscoveryJSON(result.Output)
					_, discoveryJSON, validDiscovery := validateAndRenderDiscoveryOutput(result.Output, boundBuildRunID)
					if !validDiscovery {
						discoveryOutput = nil
					}
					blockMissingRevision := boundBuildRun != nil &&
						boundBuildRun.Status == teambuild.StatusPlanning &&
						revisionToken == nil && s.TeamBuild != nil &&
						(!validDiscovery || discoveryOutput.Status == "planning_created")
					if blockMissingRevision {
						transitionCtx, cancelTransition := context.WithTimeout(runCtx, 5*time.Second)
						if blocked, transitionErr := s.TeamBuild.TransitionStatus(
							transitionCtx, tenant, boundBuildRun.BuildRunID,
							teambuild.StatusPlanning, teambuild.StatusBlocked,
							req.Agent, reasonBlueprintPlanningNoRevision,
						); transitionErr == nil {
							boundBuildRun = &blocked
							transitionReason = reasonBlueprintPlanningNoRevision
						} else {
							transitionReason = reasonBlueprintPlanningTransitionFail
						}
						cancelTransition()
						if assistantFields == nil {
							assistantFields = map[string]any{}
						}
						assistantFields["discovery_output"] = json.RawMessage(`{"status":"blocked","summary":"团队方案生成失败：本轮规划没有产出可批准的蓝图","blocking_reasons":["blueprint_planning_no_revision"]}`)
					} else if validDiscovery && discoveryJSON != nil {
						if assistantFields == nil {
							assistantFields = map[string]any{}
						}
						assistantFields["discovery_output"] = json.RawMessage(discoveryJSON)
					} else if boundBuildRun == nil {
						if assistantFields == nil {
							assistantFields = map[string]any{}
						}
						assistantFields["discovery_output"] = json.RawMessage(`{"status":"needs_clarification","summary":"需求发现未输出合法协议，需要用户补充约束或指示按当前信息收敛"}`)
					}

					outcome := terminalOutcomeForCreateTeam(
						boundBuildRun, transitionReason, discoveryOutput, revisionToken != nil,
					)
					if boundBuildRun != nil && boundBuildRun.Mode == teambuild.ModeOptimize {
						teamName := ""
						if s.OrgStore != nil {
							if team, teamErr := s.OrgStore.GetTeam(runCtx, tenant, boundBuildRun.Brief.TeamID); teamErr == nil {
								teamName = team.Name
							}
						}
						outcome.ModeNote = terminalOutcomeModeNote(boundBuildRun, teamName)
					}
					var reportSummary *blueprintSummaryMetadata
					if revisionToken != nil {
						if summary, ok := s.blueprintSummaryForAuthorization(runCtx, tenant, boundBuildRunID); ok {
							reportSummary = &summary
						}
					}
					outcome.Report = teamArchitectReportForTerminal(result.Output, outcome, reportSummary)
					terminalOutcome = &outcome
					result.Output = projectTerminalOutcome(outcome)
					if assistantFields == nil {
						assistantFields = map[string]any{}
					}
					for key, value := range terminalOutcomeAssistantFields(outcome) {
						assistantFields[key] = value
					}
					if revisionToken != nil {
						for key, value := range s.blueprintAuthorizationMetadata(runCtx, tenant, boundBuildRunID, *revisionToken) {
							assistantFields[key] = value
						}
					}
				}
			}
		}
		metadataCtx := contextWithAssistantAgent(ctx, req.Agent)
		executionMetadata := executionRecorder.snapshot()
		if terminalOutcome != nil || templateDraftReady {
			executionMetadata.Output = result.Output
		}
		metadata := mergeAssistantExecutionMetadata(
			s.buildAssistantMetadata(metadataCtx, tenant, result.Output), executionMetadata,
		)
		metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
		metadata = mergeAssistantExecutionSegments(metadata, executionRecorder.snapshotSegments())
		metadata = mergeTeamTemplateDraftMetadata(metadata, executionMetadata)
		if assistantFields != nil {
			if merged, mergeErr := mergeDiscoveryMetadata(metadata, assistantFields); mergeErr == nil {
				metadata = merged
			}
		}
		if err := s.finishTeamSessionExecution(
			ctx, teamExecution, result, userContent, metadata, runErr,
		); err != nil {
			return sendTerminalError(result.RunID, err)
		}
	}

	// Save session (include tool call metadata for session restoration).
	if result.Ran() {
		output := result.Output
		if teamExecution == nil && output != "" {
			// Embed tool call records so frontend can reconstruct tool call cards.
			savedContent := output
			if len(toolCallRecords) > 0 {
				if tcJSON, err := json.Marshal(toolCallRecords); err == nil {
					savedContent += "\n\n<!-- tool_calls:" + string(tcJSON) + " -->"
				}
			}
			// Append only the assistant turn under an atomic lock so a
			// concurrent async reflow on the same session key isn't clobbered
			// by a whole-blob rewrite. The user turn was already persisted at
			// request admission.
			newTurn := []contract.Message{{Role: "assistant", Content: savedContent}}
			_ = s.appendSessionMessages(runCtx, sessionKey, newTurn...)
		} else if teamExecution == nil {
			if resultMsgs, err := stdlib.GetMessages(result.State); err == nil && len(resultMsgs) > 0 {
				_ = stdlib.SaveSession(s.Store, sessionKey, resultMsgs)
			}
		}
		if teamExecution == nil && output != "" && conversationID != "" {
			metadataCtx := contextWithAssistantAgent(runCtx, req.Agent)
			executionMetadata := executionRecorder.snapshot()
			metadata := mergeAssistantExecutionMetadata(
				s.buildAssistantMetadata(metadataCtx, tenant, output), executionMetadata,
			)
			metadata = mergeRuntimeAssignmentMetadata(metadata, req.RuntimeAssignment)
			metadata = mergeAssistantExecutionSegments(metadata, executionRecorder.snapshotSegments())
			metadata = mergeTeamTemplateDraftMetadata(metadata, executionMetadata)
			if _, err := s.Conversations.AppendMessage(runCtx, conversation.Message{
				ConversationID: conversationID,
				WorkspaceID:    tenant,
				Role:           "assistant",
				Content:        output,
				Metadata:       metadata,
			}); err != nil {
				if runErr == nil {
					runErr = err
				}
			}
		}
		s.startOwnerMemoryFill(tenant, rec, userID, conversationID, req.Message, output, false)
	}

	// Emit yield or done event.
	if result.Yielded {
		yieldData := map[string]any{
			"run_id":             result.RunID,
			"session_id":         req.SessionID,
			"project_id":         req.ProjectID,
			"conversation_id":    conversationID,
			"user_message_id":    userMessageID,
			"runtime_assignment": req.RuntimeAssignment,
			"prompt":             "Agent is waiting for your input.",
		}
		// Forward yield_type from state if present (Mirror graph sets this).
		if yt, ok := result.State["yield_type"].(string); ok {
			yieldData["yield_type"] = yt
		}
		// Include handoff info if available (HandoffStep sets these).
		if handoffTo, ok := result.State["handoff_to"].(string); ok && handoffTo != "" {
			yieldData["handoff_to"] = handoffTo
		}
		if childGraph, ok := result.State["__child_graph"].(string); ok && childGraph != "" {
			yieldData["child_graph"] = childGraph
		}
		_ = sse.SendEvent("yield", yieldData)
		s.finishChatRequest(
			runCtx, tenant, userID, req.ClientRequestID,
			"yielded", result.RunID, yieldData,
		)
		requestFinalized = true
		return nil
	}

	if result.Ran() && runErr == nil && !streamLLM.SentBlocks {
		if blocks := filterContentBlocks(ParseBlocks(result.Output), rec.Name); len(blocks) > 0 {
			for _, block := range blocks {
				_ = sse.SendEvent("content_block", block)
			}
		}
	}

	doneData := map[string]any{
		"session_id":         req.SessionID,
		"project_id":         req.ProjectID,
		"conversation_id":    conversationID,
		"user_message_id":    userMessageID,
		"runtime_assignment": req.RuntimeAssignment,
	}
	if result.Ran() && teamExecution != nil && s.TeamBuild != nil && conversationID != "" {
		addBusinessPhaseFields(doneData, resolveBusinessPhase(runCtx, s.TeamBuild, tenant, teamExecution, result, conversationID, runErr))
	}
	if terminalOutcome != nil {
		doneData["terminal_outcome"] = *terminalOutcome
	}
	if result.Ran() {
		doneData["output"] = result.Output
		doneData["stop_reason"] = string(result.StopReason)
		doneData["usage"] = result.Usage
		doneData["run_id"] = result.RunID
		if warning := s.dispatchGroundingWarning(
			runCtx, tenant, rec, result.Output, teamExecution, agentTool, fanoutTool,
		); warning != "" {
			doneData["grounding_warning"] = warning
		}

		// Runtime info for DevMode transparency.
		ri := map[string]any{"model": rec.Model}
		if rec.Guard != nil && rec.Guard.Enabled {
			blocked, _ := result.State["__blocked"].(bool)
			ri["guard_passed"] = !blocked
		}
		if skills, ok := result.State["__active_skills"].([]string); ok && len(skills) > 0 {
			ri["active_skills"] = skills
		}
		doneData["runtime_info"] = ri
	}
	if runErr != nil {
		doneData["error"] = runErr.Error()
	}
	status := "completed"
	if runErr != nil {
		status = "failed"
	}
	s.finishChatRequest(
		runCtx, tenant, userID, req.ClientRequestID,
		status, result.RunID, doneData,
	)
	requestFinalized = true
	_ = sse.SendEvent("done", doneData)

	return nil
}

// ---------------------------------------------------------------------------
// business_phase — done-event business-phase derivation
// ---------------------------------------------------------------------------

// businessPhase is the user-facing lifecycle phase derived from persisted
// run facts, not from LLM exit status. It travels as a done-event field so
// the frontend can render the correct status banner without guessing.
type businessPhase string

const (
	businessPhaseNeedsClarification businessPhase = "needs_clarification"
	businessPhasePlanningCreated    businessPhase = "planning_created"
	businessPhaseBuilding           businessPhase = "building"
	businessPhasePublishing         businessPhase = "publishing"
	businessPhasePublished          businessPhase = "published"
	businessPhaseBlocked            businessPhase = "blocked"
	businessPhaseCancelled          businessPhase = "cancelled"
	businessPhaseGenerationFailed   businessPhase = "generation_failed"
)

var businessPhaseLabels = map[businessPhase]string{
	businessPhaseNeedsClarification: "等待补充信息",
	businessPhasePlanningCreated:    "规划已建立，等待继续",
	businessPhaseBuilding:           "构建中",
	businessPhasePublishing:         "发布中",
	businessPhasePublished:          "已发布",
	businessPhaseBlocked:            "已阻塞",
	businessPhaseCancelled:          "已取消",
	businessPhaseGenerationFailed:   "生成失败",
}

func addBusinessPhaseFields(fields map[string]any, phase businessPhase) {
	if fields == nil || phase == "" {
		return
	}
	fields["business_phase"] = phase
	fields["phase_code"] = phase
	if label := businessPhaseLabels[phase]; label != "" {
		fields["phase_label"] = label
	}
}

// resolveBusinessPhase derives the done-event business_phase for a
// create_team-intent team session. The controlling fact is the persisted
// BuildRun status, not the model's exit condition.
func resolveBusinessPhase(
	ctx context.Context,
	runs teamBuildRunReader,
	workspaceID string,
	execution *teamSessionExecution,
	result loomruntime.Result,
	conversationID string,
	runErr error,
) businessPhase {
	run, err := runs.GetLatestBuildRunByConversation(ctx, workspaceID, conversationID)
	switch {
	case err == nil:
		return buildRunStatusPhase(run.Status)
	case runErr != nil:
		return businessPhaseGenerationFailed
	}
	if hasClarificationQuestions(result.Output) {
		return businessPhaseNeedsClarification
	}
	return ""
}

func buildRunStatusPhase(status string) businessPhase {
	switch status {
	case teambuild.StatusPlanning:
		return businessPhasePlanningCreated
	case teambuild.StatusAuthorized, teambuild.StatusRoundRunning:
		return businessPhaseBuilding
	case teambuild.StatusPublishing:
		return businessPhasePublishing
	case teambuild.StatusPassed:
		return businessPhasePublished
	case teambuild.StatusBlocked:
		return businessPhaseBlocked
	case teambuild.StatusCancelled:
		return businessPhaseCancelled
	default:
		return ""
	}
}

// hasClarificationQuestions is a cheap heuristic scan: the output contains
// at least one line that suggests the architect is asking the user for input.
func hasClarificationQuestions(output string) bool {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || len(trimmed) < 20 {
		return false
	}
	return strings.Contains(trimmed, "?") || strings.Contains(trimmed, "？")
}

// teamBuildRunReader is the narrow storage surface needed for business-phase
// derivation. *teambuild.Store satisfies it.
type teamBuildRunReader interface {
	GetLatestBuildRunByConversation(ctx context.Context, workspaceID, conversationID string) (teambuild.TeamBuildRun, error)
}

var _ teamBuildRunReader = (*teambuild.Store)(nil)

// getConversationIntent returns the persistent intent of a conversation, or
// "" when the conversation or its intent is absent. When the store is
// unavailable the caller treats this as "no intent" and skips structured
// processing, so a nil receiver or error is a safe no-op.
func getConversationIntent(ctx context.Context, store conversationIntentReader, workspaceID, conversationID string) (string, error) {
	if store == nil {
		return "", nil
	}
	conversation, err := store.GetConversation(ctx, workspaceID, conversationID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(conversation.Intent), nil
}

type conversationIntentReader interface {
	GetConversation(ctx context.Context, workspaceID, conversationID string) (conversation.Conversation, error)
}

// mergeDiscoveryMetadata shallow-merges discovery protocol fields from
// assistantFields into the given metadata JSON byte slice. Discovery proto
// values replace same-named keys that already exist in the raw bytes so
// the structured output always takes precedence over the auto-generated
// assistant metadata.
func mergeDiscoveryMetadata(metadata json.RawMessage, assistantFields map[string]any) (json.RawMessage, error) {
	if len(metadata) == 0 {
		encoded, err := json.Marshal(assistantFields)
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}
	merged := map[string]any{}
	if err := json.Unmarshal(metadata, &merged); err != nil {
		return nil, fmt.Errorf("merge discovery metadata: %w", err)
	}
	for key, value := range assistantFields {
		merged[key] = value
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("merge discovery metadata: %w", err)
	}
	return encoded, nil
}

// ---------------------------------------------------------------------------
// discovery output validation — structured protocol
// ---------------------------------------------------------------------------

type discoveryOutput struct {
	Status          string   `json:"status"`
	Summary         string   `json:"summary"`
	Questions       []string `json:"questions,omitempty"`
	BuildRunID      *string  `json:"build_run_id,omitempty"`
	BlockingReasons []string `json:"blocking_reasons,omitempty"`
}

const (
	maxDiscoverySummaryChars        = 500
	maxDiscoveryQuestionChars       = 200
	maxDiscoveryQuestions           = 5
	maxDiscoveryBlockingReasonChars = 200
	maxDiscoveryBlockingReasons     = 5
)

// validateAndRenderDiscoveryOutput extracts a discoveryOutput from the raw
// LLM response and produces the validated content and metadata to persist.
// When the output is a valid discovery protocol message the rendered content
// is the summary text; the full validated object goes into metadata.
func validateAndRenderDiscoveryOutput(
	rawOutput string,
	boundBuildRunID string,
) (content string, discoveryJSON json.RawMessage, ok bool) {
	output := extractDiscoveryJSON(rawOutput)
	if output == nil {
		return "", nil, false
	}
	if err := validateDiscoveryOutput(output, boundBuildRunID); err != nil {
		return "", nil, false
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return "", nil, false
	}
	return strings.TrimSpace(output.Summary), encoded, true
}

func extractDiscoveryJSON(raw string) *discoveryOutput {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	// Try the output as a top-level JSON object first.
	var output discoveryOutput
	if json.Unmarshal([]byte(raw), &output) == nil {
		if output.Status != "" || output.Summary != "" {
			return &output
		}
	}
	// Try to find the last JSON object in the output — models often embed
	// structured output after narrative text.
	if idx := strings.LastIndex(raw, `{"status"`); idx >= 0 {
		for i := idx; i >= 0; i-- {
			if raw[i] == '\n' || i == 0 {
				candidate := raw[i:]
				if i == 0 {
					candidate = raw
				} else {
					candidate = raw[i+1:]
				}
				if json.Unmarshal([]byte(candidate), &output) == nil {
					if output.Status != "" || output.Summary != "" {
						return &output
					}
				}
				break
			}
		}
	}
	return nil
}

func validateDiscoveryOutput(output *discoveryOutput, boundBuildRunID string) error {
	if output == nil {
		return fmt.Errorf("discovery output is nil")
	}
	switch output.Status {
	case "needs_clarification", "planning_created", "blocked":
	default:
		return fmt.Errorf("discovery output status must be needs_clarification, planning_created, or blocked")
	}
	output.Summary = strings.TrimSpace(output.Summary)
	if output.Summary == "" {
		return fmt.Errorf("discovery output summary is required")
	}
	if len([]rune(output.Summary)) > maxDiscoverySummaryChars {
		return fmt.Errorf("discovery output summary exceeds %d characters", maxDiscoverySummaryChars)
	}
	if len(output.Questions) > maxDiscoveryQuestions {
		return fmt.Errorf("discovery output has more than %d questions", maxDiscoveryQuestions)
	}
	seen := make(map[string]struct{}, len(output.Questions))
	for i, q := range output.Questions {
		q = strings.TrimSpace(q)
		output.Questions[i] = q
		if q == "" {
			return fmt.Errorf("discovery output question %d is empty", i+1)
		}
		if len([]rune(q)) > maxDiscoveryQuestionChars {
			return fmt.Errorf("discovery output question %d exceeds %d characters", i+1, maxDiscoveryQuestionChars)
		}
		normalized := strings.ToLower(q)
		if _, duplicate := seen[normalized]; duplicate {
			return fmt.Errorf("discovery output question %d is duplicate", i+1)
		}
		seen[normalized] = struct{}{}
	}
	if output.BuildRunID != nil && *output.BuildRunID != "" {
		if boundBuildRunID != "" && *output.BuildRunID != boundBuildRunID {
			return fmt.Errorf("discovery output build_run_id %q does not match the bound active run", *output.BuildRunID)
		}
	}
	if output.Status != "blocked" && len(output.BlockingReasons) > 0 {
		return fmt.Errorf("discovery output blocking_reasons is only allowed when status is blocked")
	}
	if len(output.BlockingReasons) > maxDiscoveryBlockingReasons {
		return fmt.Errorf("discovery output has more than %d blocking_reasons", maxDiscoveryBlockingReasons)
	}
	for i, r := range output.BlockingReasons {
		r = strings.TrimSpace(r)
		output.BlockingReasons[i] = r
		if r == "" {
			return fmt.Errorf("discovery output blocking_reason %d is empty", i+1)
		}
		if len([]rune(r)) > maxDiscoveryBlockingReasonChars {
			return fmt.Errorf("discovery output blocking_reason %d exceeds %d characters", i+1, maxDiscoveryBlockingReasonChars)
		}
	}
	return nil
}
