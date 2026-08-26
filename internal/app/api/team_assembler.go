package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/app/metateam"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/snapshot"
	"github.com/jinyitao123/weave/internal/build/teamforge"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/teamcompiler"
)

const teamAssemblerDisabledEnv = "WEAVE_TEAM_ASSEMBLER_DISABLED"

func teamAssemblerDisabled() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(teamAssemblerDisabledEnv)), "true")
}

func (s *Server) teamInteractionAssembler() teamcompiler.TeamInteractionAssembler {
	if s.TeamAssembler != nil {
		return s.TeamAssembler
	}
	assembler := teamcompiler.NewTeamInteractionAssembler()
	if s.Pool == nil {
		return assembler
	}
	return &persistentTeamInteractionAssembler{inner: assembler, pool: s.Pool}
}

func snapshotTeamWorkers(snap snapshot.TeamRunSnapshot) ([]teamcompiler.FrozenTeamWorker, error) {
	frozenWorkers, err := snapshot.DecodeTeamWorkerSnapshot(snap.TeamWorkerSnapshot)
	if err != nil {
		return nil, fmt.Errorf("decode team worker snapshot: %w", err)
	}
	workers := make([]teamcompiler.FrozenTeamWorker, 0, len(frozenWorkers))
	for _, worker := range frozenWorkers {
		kinds := make([]teamcompiler.InteractionKind, len(worker.AllowedKinds))
		for index, kind := range worker.AllowedKinds {
			kinds[index] = teamcompiler.InteractionKind(kind)
		}
		workers = append(workers, teamcompiler.FrozenTeamWorker{
			WorkerAgentID:      worker.WorkerAgentID,
			WorkerAgentVersion: int64(worker.WorkerAgentVersion),
			Name:               worker.Name, Duty: worker.Duty, WhenToUse: worker.WhenToUse,
			ContextInstruction: worker.ContextInstruction, AllowedKinds: kinds,
			DefaultKind:       teamcompiler.InteractionKind(worker.DefaultKind),
			ResultRequirement: worker.ResultRequirement,
			EnabledAtSnapshot: worker.EnabledAtSnapshot,
			RoleProof: teamcompiler.FrozenWorkerRoleProof{
				Role:                  worker.RoleProof.Role,
				AgentContentHash:      worker.RoleProof.AgentContentHash,
				CapabilitySchema:      worker.RoleProof.CapabilitySchema,
				CapabilityContentHash: worker.RoleProof.CapabilityContentHash,
			},
		})
	}
	return workers, nil
}

// metaLeadConversationWorkers is the server-side dispatch boundary for the
// built-in meta-team lead. The full roster also contains controller-owned
// build/evaluation roles, but they are never lead-callable conversation
// workers. Filtering the frozen worker slice constrains both the published
// tool catalog and the downstream execution lookup.
func metaLeadConversationWorkers(
	workers []teamcompiler.FrozenTeamWorker,
) []teamcompiler.FrozenTeamWorker {
	result := make([]teamcompiler.FrozenTeamWorker, 0, len(workers))
	for _, worker := range workers {
		if worker.Name == metateam.ConfigEngineerName {
			result = append(result, worker)
		}
	}
	return result
}

func (s *Server) teamCompileFactory(
	ctx context.Context,
	teamExecution *teamSessionExecution,
	resume bool,
	userID string,
	memSvc *memory.Service,
) compiler.GraphFactory {
	return func(
		tenant string,
		rec *registry.AgentRecord,
		llm contract.LLM,
		tools contract.ToolDispatcher,
		opts compiler.CompileOpts,
	) (*loom.Graph, error) {
		if teamExecution == nil || rec == nil || teamExecution.Snapshot.WorkspaceID != tenant ||
			teamExecution.Snapshot.LeadAvatarID != rec.ID ||
			teamExecution.Snapshot.LeadAvatarVersion != rec.Version {
			return nil, teamcompiler.ErrTeamCompileIdentityMismatch
		}
		workers, err := snapshotTeamWorkers(teamExecution.Snapshot)
		if err != nil {
			return nil, err
		}
		if rec.Name == metateam.TeamArchitectName {
			workers = metaLeadConversationWorkers(workers)
		}
		runner := &apiLockedWorkerRunner{
			server: s, snapshot: teamExecution.Snapshot, workers: workers,
			llm: llm, memory: memSvc, userID: userID,
			conversationID: teamExecution.ConversationID,
			toolHooks:      opts.ToolHooks, beforeStepHooks: opts.BeforeStepHooks,
			afterStepHooks: opts.AfterStepHooks,
		}
		if rec.Role == "avatar" && engine.IsCLIEngine(rec.Engine) {
			runtimeBinding := *rec
			runner.llmForWorker = func(worker *registry.AgentRecord) (contract.LLM, error) {
				return s.runtimeLLMForNode(
					tenant, worker, &runtimeBinding, execution.ScopeTeamWorkerLeaf, "",
				)
			}
		}
		afterStepHooks := append([]loom.StepHook(nil), opts.AfterStepHooks...)
		var contextEnrichmentStep loom.Step
		if opts.MemoryService != nil {
			contextEnrichmentStep = memory.NewRetrieveStep(memory.RetrieveConfig{
				Service:           opts.MemoryService,
				TopK:              opts.MemoryTopK,
				Scope:             opts.MemoryScope,
				AdditionalSources: append([]memory.RetrieveSource(nil), opts.MemoryReadSources...),
			})
			if opts.AutoRemember {
				hookLLM := opts.HookLLM
				if hookLLM == nil {
					hookLLM = llm
				}
				model := rec.Model
				if model == "" {
					model = "deepseek-v4-flash"
				}
				if opts.MemoryWriteNamespace != "" {
					afterStepHooks = append(afterStepHooks, memory.AutoRememberHookForNamespace(
						hookLLM, opts.MemoryService, model, tenant, rec.Name, opts.MemoryScope,
						opts.MemoryWriteNamespace, opts.MemoryWriteSource, opts.OnMemoryUpdate,
					))
				} else {
					afterStepHooks = append(afterStepHooks, memory.AutoRememberHook(
						hookLLM, opts.MemoryService, model, tenant, rec.Name, opts.MemoryScope,
						opts.OnMemoryUpdate,
					))
				}
			}
		}
		team := teamcompiler.NewTeamCompiler(s.teamInteractionAssembler())
		if resume {
			graph, _, err := team.ResumeTeam(ctx, teamcompiler.TeamResumeInput{
				WorkspaceID: tenant, TeamID: teamExecution.Snapshot.TeamID,
				RunID: teamExecution.Snapshot.RunID, RunSnapshotID: teamExecution.Snapshot.RunID,
				Mode:           teamcompiler.TeamRunModeFreeCollaboration,
				RequestContext: opts.Context, WorkerRunner: runner,
				LLM: llm, Tools: tools, ToolHooks: opts.ToolHooks,
				BeforeStepHooks: opts.BeforeStepHooks, AfterStepHooks: afterStepHooks,
				ContextEnrichmentStep: contextEnrichmentStep,
				CheckpointStore:       s.Store,
			})
			return graph, err
		}
		graph, _, err := team.CompileTeam(ctx, teamcompiler.TeamCompileInput{
			WorkspaceID: tenant, TeamID: teamExecution.Snapshot.TeamID,
			RunID: teamExecution.Snapshot.RunID, RunSnapshotID: teamExecution.Snapshot.RunID,
			Mode: teamcompiler.TeamRunModeFreeCollaboration,
			Lead: teamcompiler.FrozenLeadRef{
				AgentID: rec.ID, AgentVersion: int64(rec.Version), Name: rec.Name,
			},
			Workers: workers, RequestContext: opts.Context, WorkerRunner: runner,
			LLM: llm, Tools: tools, ToolHooks: opts.ToolHooks,
			BeforeStepHooks: opts.BeforeStepHooks, AfterStepHooks: afterStepHooks,
			ContextEnrichmentStep: contextEnrichmentStep,
			CheckpointStore:       s.Store,
		})
		return graph, err
	}
}

type apiLockedWorkerRunner struct {
	server          *Server
	snapshot        snapshot.TeamRunSnapshot
	workers         []teamcompiler.FrozenTeamWorker
	llm             contract.LLM
	memory          *memory.Service
	userID          string
	toolHooks       []contract.ToolHook
	beforeStepHooks []loom.StepHook
	afterStepHooks  []loom.StepHook
	llmForWorker    func(*registry.AgentRecord) (contract.LLM, error)
	// conversationID is threaded from the outer chat/compile closure so the
	// worker's teamforge wiring can resolve the active build run.
	conversationID string
}

func (r *apiLockedWorkerRunner) cliWorkerStep(record *registry.AgentRecord) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		ctx = contextWithExecutionAgent(ctx, record.Name)
		message := cliWorkerContinuationPrompt(state)
		if message == "" {
			return nil, errors.New("team worker message is empty")
		}
		output, err := r.server.runVersionedEngineChat(
			ctx, record.WorkspaceID, record,
			execution.AgentExecutionStamp{
				AgentID: record.ID, AgentVersion: record.Version,
				ExecutionScope: execution.ScopeTeamWorkerLeaf,
				RunSnapshotID:  r.snapshot.RunID,
			},
			message, nil,
		)
		if err != nil {
			return nil, err
		}
		return loom.State{"output": output}, nil
	}
}

const cliWorkerHistoryMaxRunes = 24000

// cliWorkerContinuationPrompt preserves the product session's task continuity
// when a CLI worker is invoked again. Every CLI handoff is a fresh runtime
// process, so passing only last_user_message silently drops the original task,
// prior worker result, and public connection details. The first handoff stays
// byte-for-byte unchanged; later handoffs receive a bounded transcript made
// only from persisted user/assistant text before the current user turn. Tool
// messages and the lead's current-turn orchestration are intentionally omitted.
func cliWorkerContinuationPrompt(state loom.State) string {
	current := strings.TrimSpace(stdlib.GetString(state, "last_user_message", ""))
	messages, _ := stdlib.GetMessages(state)
	currentIndex := -1
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != "user" {
			continue
		}
		if current == "" {
			current = strings.TrimSpace(messages[index].Content)
		}
		currentIndex = index
		break
	}
	if current == "" {
		return ""
	}
	if currentIndex <= 0 {
		return current
	}

	entries := make([]string, 0, currentIndex)
	for _, message := range messages[:currentIndex] {
		content := strings.TrimSpace(message.Content)
		if content == "" || (message.Role != "user" && message.Role != "assistant") {
			continue
		}
		entries = append(entries, strings.ToUpper(message.Role)+":\n"+content)
	}
	if len(entries) == 0 {
		return current
	}

	entries = boundedCLIWorkerHistory(entries, cliWorkerHistoryMaxRunes)
	return "Continue the same team assignment using the persisted conversation context below. " +
		"Treat the current delegated task as the instruction to execute; use earlier turns only as context.\n\n" +
		"<team_conversation_history>\n" + strings.Join(entries, "\n\n") +
		"\n</team_conversation_history>\n\n<current_delegated_task>\n" + current +
		"\n</current_delegated_task>"
}

func boundedCLIWorkerHistory(entries []string, maxRunes int) []string {
	if len(entries) == 0 || maxRunes <= 0 {
		return nil
	}
	first := truncateCLIWorkerHistoryEntry(entries[0], maxRunes/2)
	remaining := maxRunes - len([]rune(first))
	selectedTail := make([]string, 0, len(entries)-1)
	for index := len(entries) - 1; index >= 1 && remaining > 0; index-- {
		entry := entries[index]
		entryRunes := len([]rune(entry))
		if entryRunes > remaining {
			entry = truncateCLIWorkerHistoryEntry(entry, remaining)
			entryRunes = len([]rune(entry))
		}
		selectedTail = append(selectedTail, entry)
		remaining -= entryRunes
	}
	result := make([]string, 0, len(selectedTail)+1)
	result = append(result, first)
	for index := len(selectedTail) - 1; index >= 0; index-- {
		result = append(result, selectedTail[index])
	}
	return result
}

func truncateCLIWorkerHistoryEntry(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	if maxRunes <= 32 {
		return string(runes[:maxRunes])
	}
	marker := []rune("\n...[earlier context truncated]...\n")
	available := maxRunes - len(marker)
	head := available * 2 / 3
	tail := available - head
	return string(runes[:head]) + string(marker) + string(runes[len(runes)-tail:])
}

// workerBuildRunContext resolves the active server-stored build run for one
// built-in meta-team role and derives its worker-visible typed planning state.
// Ordinary team workers receive nil and never see meta_* or build_run_id.
// Graph eligibility comes exclusively from AssembleMetaPlanningState:
// blueprint-mode planning opens the bounded declarative_v1 channel, while
// custom/template-gap and targeted-patch authority remain fail-closed.
// build_run_id is a correlation identifier, not authority by itself. This API
// path always passes nil and must never construct a patch from user text.
func (r *apiLockedWorkerRunner) workerBuildRunContext(
	ctx context.Context,
	workspaceID, agentName string,
) map[string]any {
	if r == nil || r.server == nil || r.server.TeamBuild == nil || r.conversationID == "" {
		return nil
	}
	switch agentName {
	case metateam.TeamArchitectName, metateam.ConfigEngineerName,
		metateam.GraphDesignerName, metateam.EvalDebuggerName,
		metateam.BlueprintPatchPlannerName:
	default:
		return nil
	}
	run, err := r.server.TeamBuild.GetActiveBuildRunByConversation(ctx, workspaceID, r.conversationID)
	if err != nil || run.BuildRunID == "" {
		return nil
	}
	workerContext := teamforge.AssembleMetaPlanningState(run, nil)
	workerContext["build_run_id"] = run.BuildRunID
	return workerContext
}

func (r *apiLockedWorkerRunner) StepFor(
	ctx context.Context,
	ref teamcompiler.LockedWorkerRef,
) (loom.Step, error) {
	if r == nil || r.server == nil || ref.WorkspaceID != r.snapshot.WorkspaceID ||
		ref.RunSnapshotID != r.snapshot.RunID {
		return nil, teamcompiler.ErrTeamInteractionSnapshotMismatch
	}
	var frozenWorker *teamcompiler.FrozenTeamWorker
	for index := range r.workers {
		worker := &r.workers[index]
		if worker.WorkerAgentID == ref.WorkerAgentID &&
			worker.WorkerAgentVersion == ref.WorkerAgentVersion {
			frozenWorker = worker
			break
		}
	}
	if frozenWorker == nil {
		return nil, teamcompiler.ErrTeamWorkerVersionUnavailable
	}
	if r.server.metaTeamRunDisabled(frozenWorker.Name, "") {
		return nil, errMetaTeamDisabled
	}
	record, err := r.server.Registry.GetVersion(
		ctx, ref.WorkspaceID, ref.WorkerAgentID, int(ref.WorkerAgentVersion),
	)
	if err != nil {
		return nil, fmt.Errorf("load exact team worker version: %w", err)
	}
	if err := registry.ValidateTeamWorkerAgentRecord(record); err != nil {
		return nil, teamcompiler.ErrTeamWorkerGraphIncompatible
	}
	proof, err := r.server.Descriptors.DescribeWorkerRoleProof(ctx, *record, nil)
	if err != nil {
		return nil, fmt.Errorf("verify exact team worker descriptor: %w", err)
	}
	if proof.Role != frozenWorker.RoleProof.Role ||
		proof.AgentContentHash != frozenWorker.RoleProof.AgentContentHash ||
		proof.CapabilitySchema != frozenWorker.RoleProof.CapabilitySchema ||
		proof.CapabilityContentHash != frozenWorker.RoleProof.CapabilityContentHash {
		return nil, teamcompiler.ErrTeamWorkerRoleIncompatible
	}
	if engine.IsCLIEngine(record.Engine) {
		return r.cliWorkerStep(record), nil
	}
	buildRunContext := r.workerBuildRunContext(ctx, ref.WorkspaceID, record.Name)
	workerLLM := r.llm
	if r.llmForWorker != nil {
		workerLLM, err = r.llmForWorker(record)
		if err != nil {
			return nil, fmt.Errorf("bind exact team worker runtime LLM: %w", err)
		}
	}
	broker := mcphost.NewToolBroker(r.server.mcpAccessFactory())
	workerTools := broker.Build(ctx, mcphost.ToolBrokerRequest{
		WorkspaceID: ref.WorkspaceID, Agent: record, LLM: workerLLM, Memory: r.memory,
		PlatformTools: func(contract.LLM, *memory.Service) []contract.ToolDispatcher {
			if r.server == nil {
				return nil
			}
			return r.server.teamForgePlatformTools(
				ctx, ref.WorkspaceID, r.conversationID, record.Name,
			)
		},
	})
	workerTools = teamcompiler.NewLockedWorkerDispatcher(workerTools)
	graph, err := compiler.CompileAgent(ref.WorkspaceID, record, workerLLM, workerTools, compiler.CompileOpts{
		Store:              r.server.Store,
		Context:            buildRunContext,
		SkillVersionReader: r.server.Skills,
		ToolHooks:          r.toolHooks,
		BeforeStepHooks:    r.beforeStepHooks,
		AfterStepHooks:     r.afterStepHooks,
	})
	if err != nil {
		return nil, fmt.Errorf("compile exact team worker version: %w", err)
	}
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		ctx = contextWithExecutionAgent(ctx, record.Name)
		workerState := make(loom.State, len(state)+len(buildRunContext)+5)
		for key, value := range state {
			workerState[key] = value
		}
		workerState["tenant"] = ref.WorkspaceID
		workerState["agent_name"] = record.Name
		workerState["__agent_id"] = record.ID
		workerState["__agent_version"] = record.Version
		workerState["__execution_scope"] = string(execution.ScopeTeamWorkerLeaf)
		for key, value := range buildRunContext {
			workerState[key] = value
		}
		result, err := graph.Run(ctx, workerState, r.server.Store)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, errors.New("team worker returned no result")
		}
		if _, attempted := result.State["__delegate_to"]; attempted {
			return nil, teamcompiler.ErrTeamWorkerGraphIncompatible
		}
		for _, key := range []string{
			"tenant", "agent_name", "__agent_id", "__agent_version", "__execution_scope",
		} {
			if original, ok := state[key]; ok {
				result.State[key] = original
			} else {
				delete(result.State, key)
			}
		}
		for key := range buildRunContext {
			if original, ok := state[key]; ok {
				result.State[key] = original
			} else {
				delete(result.State, key)
			}
		}
		return result.State, nil
	}, nil
}

type teamCatalogDownstream struct {
	runner *apiLockedWorkerRunner
}

func (d *teamCatalogDownstream) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{
		{
			Name:        "delegate",
			Description: "Execute one authorized team worker.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"agent":{"type":"string"},"message":{"type":"string"}},"required":["agent","message"]}`),
		},
		{
			Name:        "dispatch_parallel",
			Description: "Execute authorized team workers in parallel.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array","items":{"type":"object","properties":{"worker":{"type":"string"},"message":{"type":"string"}},"required":["worker","message"]}}},"required":["tasks"]}`),
		},
	}, nil
}

func (d *teamCatalogDownstream) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (*contract.ToolResult, error) {
	switch call.Name {
	case "delegate":
		var input struct {
			Agent   string `json:"agent"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(call.Args), &input) != nil || input.Agent == "" || input.Message == "" {
			return nil, teamcompiler.ErrTeamInteractionUnauthorized
		}
		output, err := d.run(ctx, input.Agent, input.Message)
		if err != nil {
			return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: err.Error(), IsError: true}, nil
		}
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: output}, nil
	case "dispatch_parallel":
		var input struct {
			Tasks []struct {
				Worker  string `json:"worker"`
				Message string `json:"message"`
			} `json:"tasks"`
		}
		if json.Unmarshal([]byte(call.Args), &input) != nil || len(input.Tasks) == 0 {
			return nil, teamcompiler.ErrTeamInteractionUnauthorized
		}
		results := make([]map[string]any, len(input.Tasks))
		var wait sync.WaitGroup
		for index := range input.Tasks {
			index := index
			wait.Add(1)
			go func() {
				defer wait.Done()
				task := input.Tasks[index]
				output, err := d.run(ctx, task.Worker, task.Message)
				entry := map[string]any{"worker": task.Worker, "output": output}
				if err != nil {
					entry["error"] = err.Error()
				}
				results[index] = entry
			}()
		}
		wait.Wait()
		encoded, err := json.Marshal(results)
		if err != nil {
			return nil, err
		}
		return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: string(encoded)}, nil
	default:
		return nil, teamcompiler.ErrTeamInteractionUnauthorized
	}
}

func (d *teamCatalogDownstream) run(ctx context.Context, workerID, message string) (string, error) {
	if d == nil || d.runner == nil || workerID == "" || message == "" {
		return "", teamcompiler.ErrTeamInteractionUnauthorized
	}
	var worker *teamcompiler.FrozenTeamWorker
	for index := range d.runner.workers {
		if d.runner.workers[index].WorkerAgentID == workerID {
			worker = &d.runner.workers[index]
			break
		}
	}
	if worker == nil {
		return "", teamcompiler.ErrTeamInteractionUnauthorized
	}
	step, err := d.runner.StepFor(ctx, teamcompiler.LockedWorkerRef{
		WorkspaceID:        d.runner.snapshot.WorkspaceID,
		RunSnapshotID:      d.runner.snapshot.RunID,
		WorkerAgentID:      worker.WorkerAgentID,
		WorkerAgentVersion: worker.WorkerAgentVersion,
	})
	if err != nil {
		return "", err
	}
	state, err := step(ctx, loom.State{
		"tenant":            d.runner.snapshot.WorkspaceID,
		"agent_name":        worker.Name,
		"user_id":           d.runner.userID,
		"messages":          []contract.Message{{Role: "user", Content: message}},
		"last_user_message": message,
	})
	if err != nil {
		return "", err
	}
	return stdlib.GetString(state, "output", ""), nil
}

var _ teamcompiler.LockedWorkerRunner = (*apiLockedWorkerRunner)(nil)
var _ contract.ToolDispatcher = (*teamCatalogDownstream)(nil)
