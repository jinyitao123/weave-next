package mcphost

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/executionport"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// AgentRunResult contains the complete result of an agent run. State is kept
// for callers that need to process a yielded in-process run.
type AgentRunResult struct {
	Output      string
	Yielded     bool
	YieldType   string
	RunID       string
	State       loom.State
	Usage       *engine.UsageReceipt
	Diagnostics []engine.Diagnostic
	Attempts    []engine.UsageAttempt
}

// AgentRunner runs registered agents through their configured execution path.
type AgentRunner struct {
	registry      agentRegistry
	tenant        string
	llm           contract.LLM
	store         loom.Store
	memoryService *memory.Service
	attachments   []execspec.Attachment
	compileAgent  compiler.GraphFactory
	expectedRuns  loomruntime.ExpectedRunRegistry
	RemoteExec    executionport.RemoteEngineExecutor
	Broker        *ToolBroker
	// SkillVersionReader resolves exact registry_version SkillRefs during
	// live compilation. Nil keeps legacy behavior (and fails closed for
	// agents that pin registry_version refs on this runner).
	SkillVersionReader compiler.SkillVersionReader
	InnerPlatformTools func(rec *registry.AgentRecord) []contract.ToolDispatcher
	RunLifecycleHook   loomruntime.RunLifecycleHook
	// UsageAttributionHook is passed through to loomruntime.PreparedRun so
	// callers (e.g. the team build phase) can durably associate the
	// runtime-owned run id after admission and before graph execution.
	UsageAttributionHook loomruntime.UsageAttributionHook
	TerminalResultHook   loomruntime.TerminalResultHook
	// RunTimeout bounds an in-process Loom agent run. Zero preserves the
	// two-minute interactive default; a negative value inherits only the
	// caller context, for durable workers governed by budgets and leases.
	RunTimeout time.Duration
}

// NewAgentRunner creates a runner with the same execution dependencies used by
// NewAgentToolDispatcher.
func NewAgentRunner(
	reg agentRegistry,
	tenant string,
	llm contract.LLM,
	store loom.Store,
	memSvc *memory.Service,
	attachments []execspec.Attachment,
) *AgentRunner {
	runner := &AgentRunner{
		registry: reg, tenant: tenant, llm: llm, store: store,
		memoryService: memSvc, attachments: attachments,
		compileAgent: compiler.CompileAgent,
	}
	if pgStore, ok := store.(*pgstore.PGStore); ok {
		runner.expectedRuns, _ = loomruntime.NewExpectedRunRegistry(
			storeext.New(pgStore.Pool()),
		)
	}
	return runner
}

// Run loads and executes agentName without applying delegate authorization.
func (r *AgentRunner) Run(ctx context.Context, agentName, message string) (AgentRunResult, error) {
	rec, err := r.registry.Get(ctx, r.tenant, agentName)
	if err != nil {
		return AgentRunResult{}, fmt.Errorf("agent %q not found: %v", agentName, err)
	}
	stamp, err := legacyAgentExecutionStamp(r.tenant, rec)
	if err != nil {
		return AgentRunResult{}, err
	}
	return r.runRecord(ctx, rec, stamp, message, true)
}

// RunVersion loads one immutable historical record and executes it without a
// latest-by-name fallback.
func (r *AgentRunner) RunVersion(
	ctx context.Context,
	agentID string,
	version int,
	stamp execution.AgentExecutionStamp,
	message string,
) (AgentRunResult, error) {
	if stamp.AgentID != agentID || stamp.AgentVersion != version {
		return AgentRunResult{}, fmt.Errorf("agent execution stamp does not match requested version")
	}
	rec, err := r.registry.GetVersion(ctx, r.tenant, agentID, version)
	if err != nil {
		return AgentRunResult{}, fmt.Errorf("agent version %q@%d not found: %w", agentID, version, err)
	}
	if rec == nil || rec.ID != agentID || rec.Version != version ||
		rec.WorkspaceID != r.tenant || rec.Name == "" {
		return AgentRunResult{}, fmt.Errorf("frozen agent identity does not match %q@%d", agentID, version)
	}
	return r.runRecord(ctx, rec, stamp, message, false)
}

// RunRecord executes a previously locked historical record.
func (r *AgentRunner) RunRecord(
	ctx context.Context,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	message string,
) (AgentRunResult, error) {
	return r.runRecord(ctx, rec, stamp, message, false)
}

func (r *AgentRunner) runRecord(
	ctx context.Context,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
	message string,
	resolveCurrentSkills bool,
) (AgentRunResult, error) {
	if err := validateRunnerExecutionStamp(r.tenant, rec, stamp); err != nil {
		return AgentRunResult{}, err
	}
	if stamp.ExecutionScope == execution.ScopeTeamWorkerLeaf {
		if err := registry.ValidateTeamWorkerAgentRecord(rec); err != nil {
			return AgentRunResult{}, err
		}
	}

	if engine.IsCLIEngine(rec.Engine) {
		resolved := rec
		if resolveCurrentSkills {
			resolved = r.resolveSkills(rec)
		}
		executor := r.RemoteExec
		if executor == nil {
			return AgentRunResult{}, fmt.Errorf("runtime executor is unavailable")
		}
		result, execErr := executor.ExecRemote(ctx, r.tenant, resolved, stamp, message, r.attachments)
		if execErr != nil {
			return AgentRunResult{Usage: result.Usage, Diagnostics: result.Diagnostics, Attempts: result.Attempts}, execErr
		}
		return AgentRunResult{Output: result.Output, Usage: result.Usage, Diagnostics: result.Diagnostics, Attempts: result.Attempts}, nil
	}

	if rec.RuntimeID != "" && r.RemoteExec != nil {
		resolved := rec
		if resolveCurrentSkills {
			resolved = r.resolveSkills(rec)
		}
		result, execErr := r.RemoteExec.ExecRemote(ctx, r.tenant, resolved, stamp, message, r.attachments)
		if execErr != nil {
			return AgentRunResult{Usage: result.Usage, Diagnostics: result.Diagnostics, Attempts: result.Attempts}, execErr
		}
		return AgentRunResult{Output: result.Output, Usage: result.Usage, Diagnostics: result.Diagnostics, Attempts: result.Attempts}, nil
	}

	if stamp.ExecutionScope != execution.ScopeLegacyOrchestrator {
		return AgentRunResult{}, fmt.Errorf(
			"team-scoped direct inner root terminal attribution requires durable snapshot identity",
		)
	}
	if len(rec.MCPServers) > 0 && r.Broker == nil {
		return AgentRunResult{}, fmt.Errorf("agent tool broker is unavailable")
	}
	innerRequest := ToolBrokerRequest{WorkspaceID: r.tenant, Agent: rec, LLM: r.llm, Memory: r.memoryService}
	if r.InnerPlatformTools != nil {
		builders := r.InnerPlatformTools
		innerRequest.PlatformTools = func(contract.LLM, *memory.Service) []contract.ToolDispatcher {
			return builders(rec)
		}
	}
	innerTools := r.Broker.Build(ctx, innerRequest)

	// Depth-limited like host-managed sub-agent steps: inner runs get no AgentRunner, so a
	// delegated/worker target must be a leaf — declarative worker steps fire only
	// from top-level graphs compiled by the api layer.
	compileOpts := compiler.CompileOpts{Store: r.store}
	if r.SkillVersionReader != nil {
		compileOpts.SkillVersionReader = r.SkillVersionReader
	}
	if cfg := registry.EffectiveMemoryConfig(rec); r.memoryService != nil && cfg.Enabled {
		compileOpts.MemoryService = r.memoryService
		if cfg.TopK > 0 {
			compileOpts.MemoryTopK = cfg.TopK
		}
		compileOpts.MemoryScope = cfg.Scope
		compileOpts.AutoRemember = cfg.AutoRemember
	}
	terminalAttribution, terminalAttributionErr := loomruntime.NewTerminalAttribution(
		loomruntime.TerminalAttributionInput{
			Scope:       loomruntime.TerminalAttributionLegacyUnattributed,
			WorkspaceID: r.tenant,
		},
		nil,
	)
	if terminalAttributionErr != nil {
		return AgentRunResult{}, fmt.Errorf(
			"construct direct inner root terminal attribution: %w",
			terminalAttributionErr,
		)
	}
	terminalStore := r.store
	if terminalStore == nil {
		terminalStore = loom.NewMemStore()
	}
	terminalRecordStore := loomruntime.TerminalRecordStore(
		agentRunnerTerminalRecordStore{store: terminalStore},
	)
	if pgStore, ok := r.store.(*pgstore.PGStore); ok {
		terminalRecordStore = storeext.New(pgStore.Pool())
	}
	terminalSink, terminalSinkErr := loomruntime.NewLineageTerminalSink(
		terminalRecordStore,
	)
	if terminalSinkErr != nil {
		return AgentRunResult{}, fmt.Errorf(
			"construct direct inner root terminal sink: %w",
			terminalSinkErr,
		)
	}
	if _, pgBacked := r.store.(*pgstore.PGStore); pgBacked {
		admitter, ok := r.expectedRuns.(loomruntime.FreshExpectedRunAdmitter)
		if !ok || !admitter.A4AdmissionGuaranteed() {
			return AgentRunResult{}, &loomruntime.ExpectedRunPersistenceError{
				Err: loomruntime.ErrA4FreshAdmissionUnsupported,
			}
		}
	}
	prepared, compileErr := loomruntime.Prepare(loomruntime.RunRequest{
		Tenant:              r.tenant,
		Agent:               rec,
		Stamp:               &stamp,
		TerminalAttribution: &terminalAttribution,
		Dependencies: loomruntime.Dependencies{
			LLM: r.llm, Tools: innerTools, Store: r.store, TerminalSink: terminalSink,
			ExpectedRuns:         r.expectedRuns,
			LifecycleHook:        r.RunLifecycleHook,
			UsageAttributionHook: r.UsageAttributionHook,
			TerminalResultHook:   r.TerminalResultHook,
			CompileOpts:          compileOpts, CompileAgent: r.compileAgent,
		},
	})
	if compileErr != nil {
		return AgentRunResult{}, fmt.Errorf("agent compilation failed: %v", compileErr)
	}

	innerCtx := ctx
	cancel := func() {}
	if r.RunTimeout >= 0 {
		runTimeout := r.RunTimeout
		if runTimeout == 0 {
			runTimeout = 2 * time.Minute
		}
		innerCtx, cancel = context.WithTimeout(ctx, runTimeout)
	}
	defer cancel()
	result, runErr := prepared.Run(innerCtx, loomruntime.BuildState(r.tenant, rec, loomruntime.Input{
		Messages: []contract.Message{{Role: "user", Content: message}}, LastUserMessage: message,
	}))
	if runErr != nil {
		return AgentRunResult{}, fmt.Errorf("agent %q execution failed: %w", rec.Name, runErr)
	}

	yieldType, _ := result.State["yield_type"].(string)
	output := result.Output
	if output == "" {
		if resultMsgs, messagesErr := stdlib.GetMessages(result.State); messagesErr == nil && len(resultMsgs) > 0 {
			last := resultMsgs[len(resultMsgs)-1]
			if last.Role == "assistant" {
				output = last.Content
			}
		}
	}
	if output == "" {
		output = "(agent returned no output)"
	}
	return AgentRunResult{
		Output: output, Yielded: result.Yielded, YieldType: yieldType,
		RunID: result.RunID, State: result.State,
	}, nil
}

type agentRunnerTerminalRecordStore struct {
	store loom.Store
}

func (adapter agentRunnerTerminalRecordStore) ReadValue(
	ctx context.Context,
	namespace string,
	key string,
) ([]byte, bool, error) {
	return readAgentRunnerStoreValue(ctx, adapter.store, namespace, key)
}

func (adapter agentRunnerTerminalRecordStore) ListKeys(
	ctx context.Context,
	namespace string,
) ([]string, error) {
	if adapter.store == nil {
		return nil, fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.List(ctx, namespace, "")
}

func (adapter agentRunnerTerminalRecordStore) MutateValue(
	ctx context.Context,
	namespace string,
	key string,
	mutate func(current []byte, present bool) (next []byte, err error),
) error {
	if adapter.store == nil {
		return fmt.Errorf("terminal store is unavailable")
	}
	return adapter.store.Tx(ctx, func(tx loom.Store) error {
		current, present, err := readAgentRunnerStoreValue(
			ctx,
			tx,
			namespace,
			key,
		)
		if err != nil {
			return err
		}
		next, err := mutate(current, present)
		if err != nil {
			return err
		}
		if next == nil {
			return tx.Delete(ctx, namespace, key)
		}
		return tx.Put(ctx, namespace, key, next)
	})
}

func readAgentRunnerStoreValue(
	ctx context.Context,
	store loom.Store,
	namespace string,
	key string,
) ([]byte, bool, error) {
	if store == nil {
		return nil, false, fmt.Errorf("terminal store is unavailable")
	}
	value, err := store.Get(ctx, namespace, key)
	if err == nil {
		return bytes.Clone(value), true, nil
	}
	keys, listErr := store.List(ctx, namespace, key)
	if listErr != nil {
		return nil, false, fmt.Errorf(
			"read terminal %q/%q: %v (verify absence: %w)",
			namespace,
			key,
			err,
			listErr,
		)
	}
	for _, listedKey := range keys {
		if listedKey == key {
			return nil, false, fmt.Errorf(
				"read existing terminal %q/%q: %w",
				namespace,
				key,
				err,
			)
		}
	}
	return nil, false, nil
}

func legacyAgentExecutionStamp(
	tenant string,
	rec *registry.AgentRecord,
) (execution.AgentExecutionStamp, error) {
	stamp := execution.AgentExecutionStamp{ExecutionScope: execution.ScopeLegacyOrchestrator}
	if rec != nil {
		stamp.AgentID = rec.ID
		stamp.AgentVersion = rec.Version
	}
	if err := validateRunnerExecutionStamp(tenant, rec, stamp); err != nil {
		return execution.AgentExecutionStamp{}, err
	}
	return stamp, nil
}

func validateRunnerExecutionStamp(
	tenant string,
	rec *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
) error {
	if rec == nil || rec.Name == "" || rec.ID == "" || rec.Version < 1 ||
		rec.WorkspaceID == "" || rec.WorkspaceID != tenant {
		return fmt.Errorf("locked agent record identity does not match tenant %q", tenant)
	}
	if stamp.AgentID == "" || stamp.AgentVersion < 1 || !stamp.ExecutionScope.Valid() {
		return fmt.Errorf("invalid agent execution stamp")
	}
	if stamp.LegacyScope && stamp.ExecutionScope != execution.ScopeLegacyOrchestrator {
		return fmt.Errorf("legacy execution stamp must use legacy orchestrator scope")
	}
	if stamp.AgentID != rec.ID || stamp.AgentVersion != rec.Version {
		return fmt.Errorf("agent execution stamp does not match agent record")
	}
	return nil
}

func (r *AgentRunner) resolveSkills(rec *registry.AgentRecord) *registry.AgentRecord {
	resolved := *rec
	if r.store != nil {
		resolved.Spec.Skills = compiler.ResolveSkillsFromStore(rec.Spec.Skills, r.store, r.tenant)
	}
	return &resolved
}

type compilerAgentRunnerAdapter struct{ runner *AgentRunner }

func (a compilerAgentRunnerAdapter) Run(ctx context.Context, agentName, message string) (compiler.AgentRunResult, error) {
	result, err := a.runner.Run(ctx, agentName, message)
	return compiler.AgentRunResult{
		Output: result.Output, Yielded: result.Yielded, YieldType: result.YieldType, RunID: result.RunID,
	}, err
}

// NewCompilerAgentRunner adapts an AgentRunner to the compiler.AgentRunner
// interface for injection into compiler.CompileOpts (e.g. declarative worker steps).
func NewCompilerAgentRunner(r *AgentRunner) compiler.AgentRunner {
	return compilerAgentRunnerAdapter{runner: r}
}

var _ compiler.AgentRunner = compilerAgentRunnerAdapter{}
