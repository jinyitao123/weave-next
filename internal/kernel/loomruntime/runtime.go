// Package loomruntime is the engine-agnostic core of one loom agent turn:
// initial-state assembly, agent compilation, graph execution, and result
// extraction. It owns no persistence and no transport — sessions,
// conversations, HTTP, SSE, and the task queue all stay with the caller —
// so the API server and the runtime daemon can drive the same runner with
// different injected dependencies.
//
// Dependency direction (must hold to stay importable from the daemon):
// loomruntime -> loom + contract + compiler + registry. Never api, mcphost,
// declarative, or taskqueue.
package loomruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// Input carries one turn of user input plus the identifiers the compiled
// graph reads from its initial state.
type Input struct {
	Messages        []contract.Message
	LastUserMessage string
	SessionID       string
	UserID          string
	Profile         string
	Context         map[string]any
}

// Dependencies are the injected capabilities for one run. The server passes
// its live LLM/Store and a fully assembled CompileOpts; the daemon passes
// HTTP-proxied LLM/tools, a nil-safe store, and restricted CompileOpts.
// Provider credentials and env wiring never enter this package.
type Dependencies struct {
	LLM           contract.LLM
	Tools         contract.ToolDispatcher
	Store         loom.Store
	TerminalSink  TerminalSink
	ExpectedRuns  ExpectedRunRegistry
	LifecycleHook RunLifecycleHook
	// UsageAttributionHook is invoked with the persisted RunAttemptLease
	// immediately after fresh-run admission succeeds and before any graph
	// step, stamp application, or LLM/tool execution. It lets the caller
	// durably record the runtime-owned run id (e.g. the team build usage
	// source table) so a crash between admission and the terminal marker is
	// recoverable. A hook error fails the run closed before any graph work.
	// Production callers leave it nil and the run proceeds unchanged.
	UsageAttributionHook UsageAttributionHook
	// TerminalResultHook persists caller-owned recoverable output after Loom
	// has produced the final Result and before the root terminal marker is
	// committed. A hook failure fails terminalization closed.
	TerminalResultHook TerminalResultHook
	CompileOpts        compiler.CompileOpts
	// SkillVersionReader is the production source for exact registry_version
	// SkillRefs during live compilation. Prepare applies it to CompileOpts
	// when the caller did not already set one, so every loomruntime-backed
	// path (chat, SSE, jobs, resume, scheduler, sub-agents) shares
	// the same wiring chokepoint.
	SkillVersionReader compiler.SkillVersionReader
	// CompileAgent is an optional compile seam. Production callers leave it
	// nil; focused callers such as the delegate adapter may preserve an
	// injected graph factory without taking compilation back out of the runner.
	CompileAgent compiler.GraphFactory
}

type RunLifecycleStage string

const (
	RunLifecycleStageAttemptAdmitted           RunLifecycleStage = "attempt_admitted"
	RunLifecycleStageLatestCheckpointPutBefore RunLifecycleStage = "latest_put_before"
	RunLifecycleStageLatestCheckpointPutAfter  RunLifecycleStage = "latest_put_after"
)

// RunLifecycleHook is a process-crash test seam at a completed durable
// lifecycle boundary. Production callers normally leave it nil.
type RunLifecycleHook func(context.Context, RunLifecycleStage) error

// UsageAttributionHook receives the persisted attempt lease of a fresh run
// after expected-run admission and before any graph/stamp execution. The
// lease carries the runtime-owned run id; returning an error fails the run
// closed so no LLM or tool call can start without the association durable.
type UsageAttributionHook func(context.Context, RunAttemptLease) error

type TerminalResultHook func(context.Context, RunAttemptLease, Result) error

// RunRequest contains everything needed to compile and assemble a graph for
// execution. Turn input is supplied separately because topology inspection
// prepares the same graph without running it.
type RunRequest struct {
	Tenant              string
	Agent               *registry.AgentRecord
	Stamp               *execution.AgentExecutionStamp
	TerminalAttribution *TerminalAttribution
	Dependencies        Dependencies
}

type rootTerminalSink interface {
	Write(context.Context, TerminalEntryV3) error
}

type rootTerminalSinkAdapter struct {
	sink TerminalSink
}

func (adapter rootTerminalSinkAdapter) Write(
	ctx context.Context,
	candidate TerminalEntryV3,
) error {
	return adapter.sink.Put(ctx, candidate)
}

func (adapter rootTerminalSinkAdapter) A4NormalTerminalGuaranteed() bool {
	coordinator, ok := adapter.sink.(NormalTerminalCoordinator)
	return ok && coordinator.A4NormalTerminalGuaranteed()
}

func (adapter rootTerminalSinkAdapter) CommitNormalTerminal(
	ctx context.Context,
	commit NormalTerminalCommit,
) error {
	coordinator, ok := adapter.sink.(NormalTerminalCoordinator)
	if !ok || !coordinator.A4NormalTerminalGuaranteed() {
		return normalTerminalPersistenceFailure(
			TerminalMarkerPersistenceValidate,
			commit,
			ErrA4NormalTerminalUnsupported,
		)
	}
	return coordinator.CommitNormalTerminal(ctx, commit)
}

// PreparedRun is a compiled graph bound to its execution store. It exposes
// topology metadata and the two engine entry points without exposing the
// graph itself to transport adapters.
type PreparedRun struct {
	graph                  *loom.Graph
	store                  loom.Store
	tenant                 string
	agent                  string
	stamp                  *execution.AgentExecutionStamp
	terminalAttribution    *TerminalAttribution
	terminalSink           rootTerminalSink
	expectedRuns           ExpectedRunRegistry
	lifecycleHook          RunLifecycleHook
	usageAttributionHook   UsageAttributionHook
	terminalResultHook     TerminalResultHook
	a4FreshRequired        bool
	attemptHeartbeatConfig attemptHeartbeatConfig
}

// Prepare compiles and assembles one graph without executing it.
func Prepare(req RunRequest) (PreparedRun, error) {
	if req.Agent == nil {
		return PreparedRun{}, fmt.Errorf("agent is required")
	}
	if req.Tenant == "" || req.Agent.WorkspaceID == "" || req.Agent.WorkspaceID != req.Tenant {
		return PreparedRun{}, fmt.Errorf("agent workspace does not match tenant")
	}
	var terminalAttribution *TerminalAttribution
	if req.TerminalAttribution != nil {
		if req.TerminalAttribution.workspaceID != req.Tenant {
			return PreparedRun{}, fmt.Errorf("terminal attribution workspace does not match tenant")
		}
		cloned := req.TerminalAttribution.clone()
		terminalAttribution = &cloned
	}
	var terminalSink rootTerminalSink
	if req.Dependencies.TerminalSink != nil {
		terminalSink = rootTerminalSinkAdapter{sink: req.Dependencies.TerminalSink}
	}
	expectedRuns := req.Dependencies.ExpectedRuns
	recordStore := expectedRunStoreFromTerminalSink(req.Dependencies.TerminalSink)
	_, a4FreshRequired := recordStore.(expectedRunAdmissionTxStore)
	if expectedRuns == nil {
		if recordStore != nil {
			var err error
			expectedRuns, err = NewExpectedRunRegistry(recordStore)
			if err != nil {
				return PreparedRun{}, fmt.Errorf("construct expected run registry: %w", err)
			}
		}
	}
	var stamp *execution.AgentExecutionStamp
	if req.Stamp != nil {
		if req.Stamp.AgentID == "" || req.Stamp.AgentVersion < 1 || !req.Stamp.ExecutionScope.Valid() {
			return PreparedRun{}, fmt.Errorf("invalid agent execution stamp")
		}
		if req.Stamp.LegacyScope && req.Stamp.ExecutionScope != execution.ScopeLegacyOrchestrator {
			return PreparedRun{}, fmt.Errorf("legacy execution stamp must use legacy orchestrator scope")
		}
		if req.Stamp.AgentID != req.Agent.ID || req.Stamp.AgentVersion != req.Agent.Version {
			return PreparedRun{}, fmt.Errorf("agent execution stamp does not match agent record")
		}
		copied := *req.Stamp
		stamp = &copied
	}
	compileAgent := req.Dependencies.CompileAgent
	if compileAgent == nil {
		compileAgent = compiler.CompileAgent
	}
	compileOpts := req.Dependencies.CompileOpts
	if compileOpts.SkillVersionReader == nil {
		compileOpts.SkillVersionReader = req.Dependencies.SkillVersionReader
	}
	compileOpts.BeforeStepHooks = append(
		[]loom.StepHook{bindRootUsageBeforeStep},
		compileOpts.BeforeStepHooks...,
	)
	callerWrapper := compileOpts.ExecutionLLMWrapper
	compileOpts.ExecutionLLMWrapper = func(inner contract.LLM) contract.LLM {
		if callerWrapper != nil {
			inner = callerWrapper(inner)
		}
		return newLogicalUsageLLM(inner)
	}
	executionLLM := newUsageBoundaryLLM(
		req.Dependencies.LLM,
		randomUsageAttemptID,
	)
	graph, err := compileAgent(
		req.Tenant,
		req.Agent,
		executionLLM,
		req.Dependencies.Tools,
		compileOpts,
	)
	if err != nil {
		return PreparedRun{}, err
	}
	return PreparedRun{
		graph: graph, store: req.Dependencies.Store,
		tenant: req.Tenant, agent: req.Agent.Name,
		stamp: stamp, terminalAttribution: terminalAttribution,
		terminalSink: terminalSink, expectedRuns: expectedRuns,
		lifecycleHook:          req.Dependencies.LifecycleHook,
		usageAttributionHook:   req.Dependencies.UsageAttributionHook,
		terminalResultHook:     req.Dependencies.TerminalResultHook,
		a4FreshRequired:        a4FreshRequired,
		attemptHeartbeatConfig: defaultAttemptHeartbeatConfig(),
	}, nil
}

// Name returns the compiled graph name used by topology responses and
// checkpoint keys.
func (p PreparedRun) Name() string { return p.graph.Name }

// Topology returns the compiled graph's declared topology.
func (p PreparedRun) Topology() []loom.StepInfo { return p.graph.Topology() }

// History returns retained checkpoint metadata for one run.
func (p PreparedRun) History(ctx context.Context, runID string) ([]loom.CheckpointInfo, error) {
	return p.graph.History(ctx, p.store, runID)
}

// Result is the extracted outcome of one turn.
type Result struct {
	Output     string
	StopReason loom.StopReason
	Usage      contract.Usage
	RunID      string
	LastStep   string
	State      loom.State
	Yielded    bool
}

// TerminalPersistenceError identifies a completed graph leg whose terminal
// candidate could not be assembled or persisted.
type TerminalPersistenceError struct {
	Err error
}

func (err *TerminalPersistenceError) Error() string {
	if err == nil || err.Err == nil {
		return "terminal persistence failed"
	}
	return "terminal persistence failed: " + err.Err.Error()
}

func (err *TerminalPersistenceError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// LoadRunAgentExecutionStamp reads the latest root checkpoint without
// compiling an agent. The caller-provided agent name is only a checkpoint
// locator; the stored stamp decides which immutable record and scope apply.
func LoadRunAgentExecutionStamp(
	ctx context.Context,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
) (*execution.AgentExecutionStamp, error) {
	return loadRunAgentExecutionStamp(ctx, store, tenant, agent, runID, "", nil)
}

func loadRunAgentExecutionStamp(
	ctx context.Context,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
	key string,
	expectedSeq *int64,
) (*execution.AgentExecutionStamp, error) {
	checkpoint, err := loadRunCheckpoint(
		ctx,
		store,
		tenant,
		agent,
		runID,
		key,
		expectedSeq,
	)
	if err != nil {
		return nil, err
	}
	return agentExecutionStampFromCheckpoint(checkpoint.State, runID)
}

type runCheckpoint struct {
	Schema int        `json:"schema_version"`
	RunID  string     `json:"run_id"`
	Graph  string     `json:"graph"`
	Seq    int64      `json:"seq"`
	State  loom.State `json:"state"`
}

func loadRunCheckpoint(
	ctx context.Context,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
	key string,
	expectedSeq *int64,
) (runCheckpoint, error) {
	if store == nil {
		return runCheckpoint{}, fmt.Errorf("resume checkpoint store is unavailable")
	}
	graph := tenant + ":" + agent
	namespace := "checkpoint:" + graph
	if key == "" {
		key = runID
	}
	data, err := store.Get(ctx, namespace, key)
	if err != nil {
		keys, listErr := store.List(ctx, namespace, key)
		if listErr != nil {
			return runCheckpoint{}, fmt.Errorf(
				"load checkpoint for run %q: %v (verify key absence: %w)", runID, err, listErr,
			)
		}
		for _, listedKey := range keys {
			if listedKey == key {
				return runCheckpoint{}, fmt.Errorf("load checkpoint %q for run %q: %w", key, runID, err)
			}
		}
		if expectedSeq != nil {
			return runCheckpoint{}, fmt.Errorf("%w: run %q checkpoint seq %d", loom.ErrCheckpointNotFound, runID, *expectedSeq)
		}
		return runCheckpoint{}, fmt.Errorf("%w: run %q latest checkpoint", loom.ErrCheckpointNotFound, runID)
	}
	var checkpoint runCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&checkpoint); err != nil {
		return runCheckpoint{}, fmt.Errorf("corrupt checkpoint for run %q: %w", runID, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return runCheckpoint{}, fmt.Errorf("corrupt checkpoint for run %q: trailing JSON content", runID)
	}
	if checkpoint.Schema < 0 || checkpoint.Schema > loom.CurrentCheckpointSchema {
		return runCheckpoint{}, fmt.Errorf(
			"corrupt checkpoint for run %q: unsupported schema version %d",
			runID, checkpoint.Schema,
		)
	}
	if checkpoint.RunID != runID || checkpoint.Graph != graph || checkpoint.State == nil {
		return runCheckpoint{}, fmt.Errorf("corrupt checkpoint for run %q: run ownership does not match locator", runID)
	}
	stateRunID, _ := checkpoint.State["__run_id"].(string)
	stateTenant, _ := checkpoint.State["tenant"].(string)
	stateAgent, _ := checkpoint.State["agent_name"].(string)
	if stateRunID != runID || stateTenant != tenant || stateAgent != agent {
		return runCheckpoint{}, fmt.Errorf("corrupt checkpoint for run %q: state ownership does not match locator", runID)
	}
	if expectedSeq != nil {
		stateSeq, ok := checkpointInt64(checkpoint.State["__seq"])
		if checkpoint.Seq != *expectedSeq || !ok || stateSeq != *expectedSeq {
			return runCheckpoint{}, fmt.Errorf("corrupt checkpoint for run %q: checkpoint seq does not match locator", runID)
		}
	}
	return checkpoint, nil
}

func agentExecutionStampFromCheckpoint(
	state loom.State,
	runID string,
) (*execution.AgentExecutionStamp, error) {
	rawAgentID, hasAgentID := state["__agent_id"]
	rawVersion, hasVersion := state["__agent_version"]
	rawScope, hasScope := state["__execution_scope"]
	if !hasAgentID && !hasVersion && !hasScope {
		return nil, nil
	}
	if !hasAgentID || !hasVersion {
		return nil, fmt.Errorf("corrupt checkpoint for run %q: incomplete agent execution stamp", runID)
	}
	agentID, ok := rawAgentID.(string)
	if !ok || agentID == "" {
		return nil, fmt.Errorf("corrupt checkpoint for run %q: invalid agent id stamp", runID)
	}
	versionNumber, ok := checkpointInt64(rawVersion)
	maxInt := int(^uint(0) >> 1)
	if !ok || versionNumber < 1 || versionNumber > int64(maxInt) {
		return nil, fmt.Errorf("corrupt checkpoint for run %q: invalid agent version stamp", runID)
	}
	stamp := &execution.AgentExecutionStamp{
		AgentID: agentID, AgentVersion: int(versionNumber),
		ExecutionScope: execution.ScopeLegacyOrchestrator,
	}
	if !hasScope {
		stamp.LegacyScope = true
		return stamp, nil
	}
	scopeString, ok := rawScope.(string)
	scope := execution.Scope(scopeString)
	if !ok || scopeString == "" || !scope.Valid() {
		return nil, fmt.Errorf("corrupt checkpoint for run %q: invalid execution scope stamp", runID)
	}
	stamp.ExecutionScope = scope
	return stamp, nil
}

func loadRunTerminalAttribution(
	ctx context.Context,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
) (TerminalAttribution, error) {
	checkpoint, err := loadRunCheckpoint(
		ctx,
		store,
		tenant,
		agent,
		runID,
		"",
		nil,
	)
	if err != nil {
		return TerminalAttribution{}, err
	}
	return terminalAttributionFromCheckpoint(checkpoint.State, tenant, runID)
}

func loadRunTerminalAttributionAt(
	ctx context.Context,
	store loom.Store,
	tenant string,
	agent string,
	runID string,
	seq int64,
) (TerminalAttribution, error) {
	key := fmt.Sprintf("%s/%012d", runID, seq)
	checkpoint, err := loadRunCheckpoint(
		ctx,
		store,
		tenant,
		agent,
		runID,
		key,
		&seq,
	)
	if err != nil {
		return TerminalAttribution{}, err
	}
	return terminalAttributionFromCheckpoint(checkpoint.State, tenant, runID)
}

func terminalAttributionFromCheckpoint(
	state loom.State,
	tenant string,
	runID string,
) (TerminalAttribution, error) {
	raw, exists := state[terminalAttributionStateKey]
	if exists {
		attribution, err := decodeTerminalAttributionCheckpointStamp(raw)
		if err != nil {
			return TerminalAttribution{}, fmt.Errorf(
				"corrupt checkpoint for run %q: terminal attribution: %w",
				runID,
				err,
			)
		}
		if err := validateTerminalAttributionForResult(
			attribution,
			tenant,
			Result{RunID: runID, State: state},
		); err != nil {
			return TerminalAttribution{}, fmt.Errorf(
				"corrupt checkpoint for run %q: terminal attribution: %w",
				runID,
				err,
			)
		}
		return attribution, nil
	}

	stamp, err := agentExecutionStampFromCheckpoint(state, runID)
	if err != nil {
		return TerminalAttribution{}, err
	}
	if stamp != nil && stamp.ExecutionScope != execution.ScopeLegacyOrchestrator {
		return TerminalAttribution{}, fmt.Errorf(
			"corrupt checkpoint for run %q: terminal attribution stamp is missing for execution scope %q",
			runID,
			stamp.ExecutionScope,
		)
	}
	parentRunID, parentSeq, err := terminalCanonicalParentFromState(state)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"corrupt checkpoint for run %q: terminal attribution: %w",
			runID,
			err,
		)
	}
	attribution, err := NewTerminalAttribution(TerminalAttributionInput{
		Scope:       TerminalAttributionLegacyUnattributed,
		WorkspaceID: tenant,
		ParentRunID: parentRunID.pointer(),
		ParentSeq:   parentSeq.pointer(),
	}, nil)
	if err != nil {
		return TerminalAttribution{}, fmt.Errorf(
			"restore historical legacy terminal attribution for run %q: %w",
			runID,
			err,
		)
	}
	return attribution, nil
}

func checkpointInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case json.Number:
		parsed, err := strconv.ParseInt(value.String(), 10, 64)
		return parsed, err == nil
	case float64:
		const maxExactFloatInteger = float64(1 << 53)
		if value < 0 || value > maxExactFloatInteger || float64(int64(value)) != value {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
}

// Ran reports whether a graph execution produced a state snapshot. It is the
// discriminator for the partial-result contract of Run: err != nil && Ran()
// means the graph stopped with an error mid-run and Result still carries the
// partial state; err != nil && !Ran() means nothing executed at all.
func (r Result) Ran() bool { return r.State != nil }

// BuildState assembles the initial graph state for one turn. This key set is
// the contract between weave and compiled graphs — dropping a key here
// silently changes prompt assembly (__profile, context) or memory scoping
// (user_id, session_id), so it lives in exactly one place.
func BuildState(tenant string, rec *registry.AgentRecord, in Input) loom.State {
	state := loom.State{
		"messages":          in.Messages,
		"last_user_message": in.LastUserMessage,
		"session_id":        in.SessionID,
		"tenant":            tenant,
		"user_id":           in.UserID,
		"agent_name":        rec.Name,
		"__profile":         in.Profile,
		"context":           in.Context,
	}
	ensureRunStartedAt(state, time.Now())
	initializeUsageAccumulator(state)
	return state
}

func ensureRunStartedAt(state loom.State, startedAt time.Time) {
	if _, exists := state["__run_started_at"]; !exists {
		state["__run_started_at"] = startedAt.UTC().Format(time.RFC3339Nano)
	}
}

func canonicalFreshRunStartedAt(state loom.State, fallback time.Time) (string, error) {
	raw, exists := state["__run_started_at"]
	if !exists {
		startedAt := fallback.UTC().Format(time.RFC3339Nano)
		state["__run_started_at"] = startedAt
		return startedAt, nil
	}
	startedAt, ok := raw.(string)
	if !ok || startedAt == "" {
		return "", fmt.Errorf("__run_started_at must be a non-empty canonical RFC3339Nano string")
	}
	if err := canonicalRunStartedAt(startedAt); err != nil {
		return "", fmt.Errorf("__run_started_at: %w", err)
	}
	if err := validateA4AdmissionReceiptTime(startedAt); err != nil {
		return "", fmt.Errorf("__run_started_at: %w", err)
	}
	return startedAt, nil
}

// FromRunResult maps a loom RunResult onto the runner Result. A nil input
// yields the zero Result (Ran() == false).
func FromRunResult(res *loom.RunResult) Result {
	if res == nil {
		return Result{}
	}
	out := Result{
		State:      res.State,
		StopReason: res.StopReason,
		RunID:      res.RunID,
		LastStep:   res.LastStep,
		Yielded:    res.Yielded,
	}
	out.Output, _ = res.State["output"].(string)
	out.Usage, _ = res.State["usage"].(contract.Usage)
	return out
}

func finish(res *loom.RunResult, runErr error) (Result, error) {
	if runErr != nil && res == nil {
		return Result{}, runErr
	}
	return FromRunResult(res), runErr
}

// Run executes a prepared graph from its entry step and returns the shared
// runner result contract.
func (p PreparedRun) Run(ctx context.Context, input loom.State) (Result, error) {
	if p.terminalAttribution == nil {
		return Result{}, fmt.Errorf("terminal attribution is required for fresh run")
	}
	if p.terminalSink == nil {
		return Result{}, fmt.Errorf("terminal sink is required for fresh run")
	}
	coordinator, err := p.normalTerminalCoordinator()
	if err != nil {
		return Result{}, err
	}
	wallStart := time.Now()
	input = cloneState(input)
	runStartedAt, err := canonicalFreshRunStartedAt(input, wallStart)
	if err != nil {
		return Result{}, &ExpectedRunPersistenceError{Err: err}
	}
	authoritativeStartedAt, err := time.Parse(time.RFC3339Nano, runStartedAt)
	if err != nil {
		return Result{}, &ExpectedRunPersistenceError{
			Err: fmt.Errorf("parse authoritative __run_started_at: %w", err),
		}
	}
	clearExecutionStamp(input)
	clearUsageAccumulator(input)
	initializeUsageAccumulator(input)
	heartbeat, err := p.freshAttemptHeartbeat()
	if err != nil {
		return Result{}, err
	}
	var admissionAnchor time.Time
	if heartbeat != nil {
		admissionAnchor = p.heartbeatConfig().Clock.Now()
	}
	lease, err := p.bindFreshExpectedRun(
		ctx,
		input,
		*p.terminalAttribution,
		runStartedAt,
	)
	if err != nil {
		return Result{}, err
	}
	if lease != nil {
		if err := p.runLifecycleHook(
			ctx,
			RunLifecycleStageAttemptAdmitted,
		); err != nil {
			return Result{}, err
		}
		if err := p.runUsageAttributionHook(ctx, *lease); err != nil {
			return Result{}, err
		}
		authoritativeStartedAt, err = time.Parse(
			time.RFC3339Nano,
			lease.RunStartedAt,
		)
		if err != nil {
			return Result{}, &ExpectedRunPersistenceError{
				Err: fmt.Errorf("parse committed lease run_started_at: %w", err),
			}
		}
	}
	if p.stamp != nil {
		input["__agent_id"] = p.stamp.AgentID
		input["__agent_version"] = p.stamp.AgentVersion
		input["__execution_scope"] = string(p.stamp.ExecutionScope)
	}
	ctx = withUsageRunScope(ctx)
	graphCtx := ctx
	var heartbeatScope *attemptHeartbeatScope
	if lease != nil {
		heartbeatScope, err = startAttemptHeartbeat(
			ctx,
			heartbeat,
			*lease,
			admissionAnchor,
			p.heartbeatConfig(),
		)
		if err != nil {
			return Result{}, err
		}
		graphCtx = heartbeatScope.Context()
	}
	runID, _ := input["__run_id"].(string)
	terminalizer := &preparedRunTerminalizer{
		prepared:      p,
		startedAt:     authoritativeStartedAt,
		attribution:   *p.terminalAttribution,
		coordinator:   coordinator,
		lease:         lease,
		heartbeat:     heartbeatScope,
		terminalCtx:   ctx,
		expectedRun:   runID,
		expectedGraph: p.graph.Name,
	}
	result, runErr := finish(p.graph.RunWithLifecycle(
		graphCtx,
		input,
		p.store,
		loom.LifecycleHooks{
			Terminalizer:       terminalizer,
			CheckpointObserver: preparedCheckpointObserver{prepared: p},
		},
	))
	if heartbeatErr := heartbeatScope.Stop(); heartbeatErr != nil &&
		!errors.Is(runErr, heartbeatErr) {
		return finishWithHeartbeatError(result, runErr, heartbeatErr)
	}
	return result, runErr
}

func (p PreparedRun) runLifecycleHook(
	ctx context.Context,
	stage RunLifecycleStage,
) error {
	if p.lifecycleHook == nil {
		return nil
	}
	if err := p.lifecycleHook(ctx, stage); err != nil {
		return fmt.Errorf("run lifecycle stage %s: %w", stage, err)
	}
	return nil
}

func (p PreparedRun) runUsageAttributionHook(
	ctx context.Context,
	lease RunAttemptLease,
) error {
	if p.usageAttributionHook == nil {
		return nil
	}
	if err := p.usageAttributionHook(ctx, lease); err != nil {
		return fmt.Errorf(
			"usage attribution for run %q in workspace %q: %w",
			lease.RunID,
			lease.WorkspaceID,
			err,
		)
	}
	return nil
}

func (p PreparedRun) bindFreshExpectedRun(
	ctx context.Context,
	state loom.State,
	attribution TerminalAttribution,
	runStartedAt string,
) (*RunAttemptLease, error) {
	if p.expectedRuns == nil {
		return nil, &ExpectedRunPersistenceError{Err: fmt.Errorf("expected run registry is required for fresh run")}
	}
	runID := uuid.NewString()
	if attribution.runSnapshotID.present && !attribution.aggregationParentRunID.present {
		runID = attribution.runSnapshotID.value
	}
	if existing, present := state["__run_id"]; present {
		existingRunID, ok := existing.(string)
		if !ok || existingRunID == "" {
			return nil, fmt.Errorf("fresh run __run_id does not match runtime-owned identity")
		}
		if attribution.aggregationParentRunID.present {
			if _, err := uuid.Parse(existingRunID); err != nil {
				return nil, fmt.Errorf("fresh aggregation run __run_id is not a valid UUID")
			}
			runID = existingRunID
		} else if existingRunID != runID {
			return nil, fmt.Errorf("fresh run __run_id does not match runtime-owned identity")
		}
	}
	state["__run_id"] = runID
	if err := bindTerminalAttribution(state, attribution); err != nil {
		return nil, err
	}
	input := attribution.inputCopy()
	record := ExpectedRunRecordV1{
		SchemaVersion:          expectedRunRecordSchemaV1,
		WorkspaceID:            input.WorkspaceID,
		RunID:                  runID,
		Agent:                  p.agent,
		AttributionScope:       input.Scope,
		TeamID:                 input.TeamID,
		WorkflowID:             input.WorkflowID,
		WorkflowVersion:        input.WorkflowVersion,
		RunSnapshotID:          input.RunSnapshotID,
		ParentRunID:            input.ParentRunID,
		ParentSeq:              input.ParentSeq,
		AggregationParentRunID: input.AggregationParentRunID,
		TaskGroupID:            input.TaskGroupID,
		RegisteredAt:           time.Now().UTC(),
	}
	return p.admitFreshExpectedRun(ctx, record, runStartedAt)
}

func (p PreparedRun) admitFreshExpectedRun(
	ctx context.Context,
	record ExpectedRunRecordV1,
	runStartedAt string,
) (*RunAttemptLease, error) {
	if p.expectedRuns == nil {
		return nil, &ExpectedRunPersistenceError{
			Err: fmt.Errorf("expected run registry is required for fresh run"),
		}
	}
	attemptID := uuid.New()
	admitter, hasAdmitter := p.expectedRuns.(FreshExpectedRunAdmitter)
	lifecycle, hasLifecycle := p.expectedRuns.(AttemptLeaseLifecycle)
	if p.a4FreshRequired &&
		(!hasAdmitter || !admitter.A4AdmissionGuaranteed()) {
		return nil, &ExpectedRunPersistenceError{Err: ErrA4FreshAdmissionUnsupported}
	}
	if p.a4FreshRequired &&
		(!hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed()) {
		return nil, &ExpectedRunPersistenceError{
			Err: ErrA4AttemptLeaseLifecycleUnsupported,
		}
	}
	if hasAdmitter && admitter.A4AdmissionGuaranteed() {
		if !hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed() {
			return nil, &ExpectedRunPersistenceError{
				Err: ErrA4AttemptLeaseLifecycleUnsupported,
			}
		}
		lease, err := admitter.AdmitFresh(ctx, FreshExpectedRunAdmission{
			Record:       record,
			GraphName:    p.graph.Name,
			RunStartedAt: runStartedAt,
			AttemptID:    attemptID,
			LeaseTTL:     120 * time.Second,
		})
		if err != nil {
			return nil, &ExpectedRunPersistenceError{Err: err}
		}
		return &lease, nil
	}
	if err := p.expectedRuns.Register(ctx, record); err != nil {
		return nil, &ExpectedRunPersistenceError{Err: err}
	}
	return nil, nil
}

func (p PreparedRun) heartbeatConfig() attemptHeartbeatConfig {
	config := p.attemptHeartbeatConfig
	if config.Clock == nil {
		return defaultAttemptHeartbeatConfig()
	}
	return config
}

func (p PreparedRun) freshAttemptHeartbeat() (AttemptLeaseHeartbeat, error) {
	admitter, hasAdmitter := p.expectedRuns.(FreshExpectedRunAdmitter)
	a4Admitter := hasAdmitter && admitter.A4AdmissionGuaranteed()
	if p.a4FreshRequired && !a4Admitter {
		return nil, nil
	}
	lifecycle, hasLifecycle := p.expectedRuns.(AttemptLeaseLifecycle)
	if a4Admitter &&
		(!hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed()) {
		return nil, nil
	}
	return p.attemptHeartbeat(p.a4FreshRequired || a4Admitter)
}

func (p PreparedRun) resumeAttemptHeartbeat() (AttemptLeaseHeartbeat, error) {
	return p.attemptHeartbeat(true)
}

func (p PreparedRun) attemptHeartbeat(
	required bool,
) (AttemptLeaseHeartbeat, error) {
	if !required {
		return nil, nil
	}
	fail := func(err error) (AttemptLeaseHeartbeat, error) {
		return nil, &AttemptLeaseHeartbeatError{
			Stage:       AttemptLeaseHeartbeatValidate,
			WorkspaceID: p.tenant,
			Err:         err,
		}
	}
	heartbeat, ok := p.expectedRuns.(AttemptLeaseHeartbeat)
	if !ok || !heartbeat.A4AttemptLeaseHeartbeatGuaranteed() {
		return fail(ErrA4AttemptLeaseHeartbeatUnsupported)
	}
	if err := validateAttemptHeartbeatConfig(p.heartbeatConfig()); err != nil {
		return fail(err)
	}
	return heartbeat, nil
}

func (p PreparedRun) handoffYieldedAttempt(
	ctx context.Context,
	lease *RunAttemptLease,
	result Result,
	terminalErr error,
) error {
	if terminalErr != nil || !result.Yielded || lease == nil {
		return terminalErr
	}
	lifecycle, ok := p.expectedRuns.(AttemptLeaseLifecycle)
	if !ok || !lifecycle.A4AttemptLeaseLifecycleGuaranteed() {
		return ErrA4AttemptLeaseLifecycleUnsupported
	}
	if _, err := lifecycle.MarkAttemptYielded(ctx, attemptLeaseOwner(*lease)); err != nil {
		return err
	}
	return nil
}

func (p PreparedRun) normalTerminalCoordinator() (
	NormalTerminalCoordinator,
	error,
) {
	if !p.a4FreshRequired {
		return nil, nil
	}
	coordinator, ok := p.terminalSink.(NormalTerminalCoordinator)
	if ok && coordinator.A4NormalTerminalGuaranteed() {
		return coordinator, nil
	}
	markerErr := &TerminalMarkerPersistenceError{
		Stage:       TerminalMarkerPersistenceValidate,
		WorkspaceID: p.tenant,
		Err:         ErrA4NormalTerminalUnsupported,
	}
	return nil, &TerminalPersistenceError{Err: markerErr}
}

func finishWithTerminalError(
	result Result,
	runErr error,
	terminalErr error,
) (Result, error) {
	if terminalErr == nil {
		return result, runErr
	}
	persistenceErr := &TerminalPersistenceError{Err: terminalErr}
	if runErr == nil {
		return result, persistenceErr
	}
	return result, errors.Join(runErr, persistenceErr)
}

func finishWithHeartbeatError(
	result Result,
	runErr error,
	heartbeatErr error,
) (Result, error) {
	if heartbeatErr == nil {
		return result, runErr
	}
	if runErr == nil {
		return result, heartbeatErr
	}
	return result, errors.Join(runErr, heartbeatErr)
}

func (p PreparedRun) persistRootTerminal(
	ctx context.Context,
	startedAt time.Time,
	endedAt time.Time,
	result Result,
	runErr error,
	attribution TerminalAttribution,
	coordinator NormalTerminalCoordinator,
	lease *RunAttemptLease,
) (bool, error) {
	candidate, err := AssembleTerminalV3(TerminalAssemblyInput{
		Tenant:      p.tenant,
		Agent:       p.agent,
		StartedAt:   startedAt,
		EndedAt:     endedAt,
		Result:      result,
		RunErr:      runErr,
		Attribution: attribution,
	})
	if err != nil {
		return coordinator != nil, fmt.Errorf(
			"assemble root terminal: %w",
			err,
		)
	}
	coordinated, err := persistNormalTerminalCandidate(
		ctx,
		p.terminalSink,
		coordinator,
		lease,
		candidate,
	)
	if err != nil {
		return coordinated, fmt.Errorf("write root terminal: %w", err)
	}
	return coordinated, nil
}

func persistNormalTerminalCandidate(
	ctx context.Context,
	sink rootTerminalSink,
	coordinator NormalTerminalCoordinator,
	lease *RunAttemptLease,
	candidate TerminalEntryV3,
) (bool, error) {
	if coordinator != nil {
		if lease == nil {
			return true, normalTerminalPersistenceFailure(
				TerminalMarkerPersistenceValidate,
				NormalTerminalCommit{Candidate: candidate},
				ErrA4NormalTerminalUnsupported,
			)
		}
		return true, coordinator.CommitNormalTerminal(
			ctx,
			NormalTerminalCommit{
				Candidate: candidate,
				Owner:     attemptLeaseOwner(*lease),
			},
		)
	}
	if sink == nil {
		return false, fmt.Errorf("terminal sink is required")
	}
	return false, sink.Write(ctx, candidate)
}

func persistedRunStartedAt(state loom.State, fallback time.Time) time.Time {
	if raw, ok := state["__run_started_at"].(string); ok {
		if persisted, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return persisted
		}
	}
	return fallback
}

// Resume restores a prepared graph from runID and returns the same Result
// contract as Run.
func (p PreparedRun) Resume(ctx context.Context, runID string, input loom.State) (Result, error) {
	if p.terminalSink == nil {
		return Result{}, fmt.Errorf("terminal sink is required for resume")
	}
	coordinator, err := p.normalTerminalCoordinator()
	if err != nil {
		return Result{}, err
	}
	attribution, err := loadRunTerminalAttribution(
		ctx,
		p.store,
		p.tenant,
		p.agent,
		runID,
	)
	if err != nil {
		return Result{}, err
	}
	wallStart := time.Now()
	var lease *RunAttemptLease
	lifecycle, hasLifecycle := p.expectedRuns.(AttemptLeaseLifecycle)
	if p.a4FreshRequired &&
		(!hasLifecycle || !lifecycle.A4AttemptLeaseLifecycleGuaranteed()) {
		return Result{}, &ExpectedRunPersistenceError{
			Err: ErrA4AttemptLeaseLifecycleUnsupported,
		}
	}
	if hasLifecycle && lifecycle.A4AttemptLeaseLifecycleGuaranteed() {
		heartbeat, err := p.resumeAttemptHeartbeat()
		if err != nil {
			return Result{}, err
		}
		admissionAnchor := p.heartbeatConfig().Clock.Now()
		admitted, err := lifecycle.AdmitResume(ctx, ResumeAttemptAdmission{
			WorkspaceID: p.tenant,
			RunID:       runID,
			GraphName:   p.graph.Name,
			AttemptID:   uuid.New(),
			LeaseTTL:    120 * time.Second,
		})
		if err != nil {
			return Result{}, &ExpectedRunPersistenceError{Err: err}
		}
		lease = &admitted
		if err := p.runLifecycleHook(
			ctx,
			RunLifecycleStageAttemptAdmitted,
		); err != nil {
			return Result{}, err
		}
		ctx = withUsageRunScope(ctx)
		heartbeatScope, err := startAttemptHeartbeat(
			ctx,
			heartbeat,
			admitted,
			admissionAnchor,
			p.heartbeatConfig(),
		)
		if err != nil {
			return Result{}, err
		}
		input = cloneState(input)
		clearExecutionStamp(input)
		clearUsageAccumulator(input)
		delete(input, terminalAttributionStateKey)
		startedAt, parseErr := time.Parse(time.RFC3339Nano, admitted.RunStartedAt)
		if parseErr != nil {
			_ = heartbeatScope.Stop()
			return Result{}, &ExpectedRunPersistenceError{
				Err: fmt.Errorf("parse committed lease run_started_at: %w", parseErr),
			}
		}
		terminalizer := &preparedRunTerminalizer{
			prepared:      p,
			startedAt:     startedAt,
			attribution:   attribution,
			coordinator:   coordinator,
			lease:         lease,
			heartbeat:     heartbeatScope,
			terminalCtx:   ctx,
			expectedRun:   runID,
			expectedGraph: p.graph.Name,
		}
		result, runErr := finish(p.graph.ResumeWithLifecycle(
			heartbeatScope.Context(),
			runID,
			input,
			p.store,
			loom.LifecycleHooks{
				Terminalizer:       terminalizer,
				CheckpointObserver: preparedCheckpointObserver{prepared: p},
			},
		))
		if heartbeatErr := heartbeatScope.Stop(); heartbeatErr != nil &&
			!errors.Is(runErr, heartbeatErr) {
			return finishWithHeartbeatError(result, runErr, heartbeatErr)
		}
		return result, runErr
	}
	input = cloneState(input)
	clearExecutionStamp(input)
	clearUsageAccumulator(input)
	delete(input, terminalAttributionStateKey)
	ctx = withUsageRunScope(ctx)
	result, runErr := finish(p.graph.Resume(ctx, runID, input, p.store))
	return p.finishResumedRoot(
		ctx,
		wallStart,
		lease,
		result,
		runErr,
		attribution,
		coordinator,
	)
}

func (p PreparedRun) finishResumedRoot(
	ctx context.Context,
	wallStart time.Time,
	lease *RunAttemptLease,
	result Result,
	runErr error,
	attribution TerminalAttribution,
	coordinator NormalTerminalCoordinator,
) (Result, error) {
	if result.Ran() {
		startedAt := persistedRunStartedAt(result.State, wallStart)
		if lease != nil {
			parsedStartedAt, err := time.Parse(time.RFC3339Nano, lease.RunStartedAt)
			if err != nil {
				return finishWithTerminalError(
					result,
					runErr,
					fmt.Errorf("parse committed lease run_started_at: %w", err),
				)
			}
			startedAt = parsedStartedAt
		}
		coordinated, terminalErr := p.persistRootTerminal(
			ctx,
			startedAt,
			time.Now(),
			result,
			runErr,
			attribution,
			coordinator,
			lease,
		)
		if !coordinated {
			terminalErr = p.handoffYieldedAttempt(
				ctx,
				lease,
				result,
				terminalErr,
			)
		}
		return finishWithTerminalError(result, runErr, terminalErr)
	}
	return result, runErr
}

// ResumeAt forks a new run from an immutable historical checkpoint and
// records its terminal result through the same path as Run and Resume.
func (p PreparedRun) ResumeAt(ctx context.Context, runID string, seq int64, input loom.State) (Result, error) {
	if p.terminalSink == nil {
		return Result{}, fmt.Errorf("terminal sink is required for resume-at")
	}
	coordinator, err := p.normalTerminalCoordinator()
	if err != nil {
		return Result{}, err
	}
	if _, err := loadRunTerminalAttributionAt(
		ctx,
		p.store,
		p.tenant,
		p.agent,
		runID,
		seq,
	); err != nil {
		return Result{}, err
	}
	parentRunID := runID
	parentSeq := seq
	attribution, err := NewTerminalAttribution(TerminalAttributionInput{
		Scope:       TerminalAttributionLegacyUnattributed,
		WorkspaceID: p.tenant,
		ParentRunID: &parentRunID,
		ParentSeq:   &parentSeq,
	}, nil)
	if err != nil {
		return Result{}, fmt.Errorf("construct fork terminal attribution: %w", err)
	}
	wallStart := time.Now().UTC()
	runStartedAt := wallStart.Format(time.RFC3339Nano)
	input = cloneState(input)
	clearExecutionStamp(input)
	clearUsageAccumulator(input)
	if err := StoreUsageAccumulator(input, NewUsageAccumulator()); err != nil {
		return Result{}, fmt.Errorf(
			"initialize fork usage accumulator: %w",
			err,
		)
	}
	delete(input, terminalAttributionStateKey)
	deletedKeys := make([]string, 0, 3)
	if p.stamp == nil {
		deletedKeys = append(
			deletedKeys,
			"__agent_id",
			"__agent_version",
			"__execution_scope",
		)
	} else {
		input["__agent_id"] = p.stamp.AgentID
		input["__agent_version"] = p.stamp.AgentVersion
		input["__execution_scope"] = string(execution.ScopeLegacyOrchestrator)
	}
	input["__deleted_keys"] = deletedKeys
	if err := bindTerminalAttribution(input, attribution); err != nil {
		return Result{}, err
	}
	input["__run_started_at"] = runStartedAt
	heartbeat, err := p.freshAttemptHeartbeat()
	if err != nil {
		return Result{}, err
	}
	var admissionAnchor time.Time
	if heartbeat != nil {
		admissionAnchor = p.heartbeatConfig().Clock.Now()
	}
	lifecycle := &forkRunLifecycle{
		prepared:        p,
		attribution:     attribution,
		coordinator:     coordinator,
		runStartedAt:    runStartedAt,
		startedAt:       wallStart,
		heartbeat:       heartbeat,
		admissionAnchor: admissionAnchor,
		terminalCtx:     ctx,
	}
	ctx = context.WithValue(ctx, forkUsageContextKey{}, true)
	ctx = withUsageRunScope(ctx)
	result, runErr := finish(p.graph.ResumeAtWithLifecycle(
		ctx,
		runID,
		seq,
		uuid.NewString(),
		input,
		p.store,
		loom.LifecycleHooks{
			RunAllocated:       lifecycle,
			ExecutionContext:   lifecycle,
			Terminalizer:       lifecycle,
			CheckpointObserver: preparedCheckpointObserver{prepared: p},
		},
	))
	if heartbeatErr := lifecycle.stopHeartbeat(); heartbeatErr != nil &&
		!errors.Is(runErr, heartbeatErr) {
		return finishWithHeartbeatError(result, runErr, heartbeatErr)
	}
	return result, runErr
}

type forkUsageContextKey struct{}

func bindRootUsageBeforeStep(
	ctx context.Context,
	step string,
	state loom.State,
) error {
	if err := attemptHeartbeatLossCause(ctx); err != nil {
		return err
	}
	if err := initializeForkUsageBeforeStep(ctx, step, state); err != nil {
		return err
	}
	return bindUsageBeforeStep(ctx, step, state)
}

func initializeForkUsageBeforeStep(
	ctx context.Context,
	_ string,
	state loom.State,
) error {
	fork, _ := ctx.Value(forkUsageContextKey{}).(bool)
	if !fork {
		return nil
	}
	if _, exists := state[usageAccumulatorStateKey]; exists {
		return nil
	}
	if err := StoreUsageAccumulator(state, NewUsageAccumulator()); err != nil {
		return fmt.Errorf("initialize fork usage accumulator: %w", err)
	}
	return nil
}

func cloneState(input loom.State) loom.State {
	cloned := make(loom.State, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func clearExecutionStamp(input loom.State) {
	delete(input, "__agent_id")
	delete(input, "__agent_version")
	delete(input, "__execution_scope")
}

// Run compiles rec and executes one turn.
//
// Error contract (bit-compatible with the inline copies this replaced):
//   - compile failure: zero Result plus an error prefixed
//     "agent compilation failed: ";
//   - run failure with no result: zero Result plus the run error;
//   - partial result (graph stopped with an error but produced state, e.g.
//     StopError/StopMaxIter/StopBudget): populated Result AND the run error.
//     Callers distinguish this case with Result.Ran().
func Run(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	in Input,
	terminalAttribution TerminalAttribution,
	deps Dependencies,
) (Result, error) {
	var stamp *execution.AgentExecutionStamp
	if rec != nil && rec.ID != "" && rec.Version > 0 {
		stamp = &execution.AgentExecutionStamp{
			AgentID: rec.ID, AgentVersion: rec.Version,
			ExecutionScope: execution.ScopeLegacyOrchestrator,
		}
	}
	prepared, err := Prepare(RunRequest{
		Tenant:              tenant,
		Agent:               rec,
		Stamp:               stamp,
		TerminalAttribution: &terminalAttribution,
		Dependencies:        deps,
	})
	if err != nil {
		return Result{}, fmt.Errorf("agent compilation failed: %w", err)
	}
	return prepared.Run(ctx, BuildState(tenant, rec, in))
}

// Resume compiles rec, restores runID from the injected store, and returns
// the same Result contract as Run. The caller owns construction of resume
// input because it is merged into checkpoint state by loom.
func Resume(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	runID string,
	input loom.State,
	deps Dependencies,
) (Result, error) {
	prepared, err := Prepare(RunRequest{Tenant: tenant, Agent: rec, Dependencies: deps})
	if err != nil {
		return Result{}, fmt.Errorf("agent compilation failed: %w", err)
	}
	return prepared.Resume(ctx, runID, input)
}
