package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/grounding"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/otel"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/skills"
)

// GraphFactory creates a custom Graph for agents with non-standard topologies.
// Called instead of the default compilation logic when AgentRecord.GraphType
// matches a registered factory name.
type GraphFactory func(tenant string, rec *registry.AgentRecord, llm contract.LLM, tools contract.ToolDispatcher, opts CompileOpts) (*loom.Graph, error)

// SkillVersionReader resolves one exact immutable SkillVersion for live
// compilation. internal/skills.Store satisfies this interface structurally;
// the narrow interface lives here so internal/skills never imports the
// compiler (no import cycle) and tests can inject fakes.
type SkillVersionReader interface {
	GetVersion(ctx context.Context, workspaceID, skillID string, version int64) (*skills.SkillVersion, error)
}

var (
	graphFactoriesMu sync.RWMutex
	graphFactories   = map[string]GraphFactory{}
)

// RegisterGraphFactory registers a custom graph factory for the given graph type.
// Must be called before server starts (typically in init() or main setup).
func RegisterGraphFactory(graphType string, factory GraphFactory) {
	graphFactoriesMu.Lock()
	defer graphFactoriesMu.Unlock()
	graphFactories[graphType] = factory
}

// lookupFactory returns a registered factory, if any.
func lookupFactory(graphType string) (GraphFactory, bool) {
	graphFactoriesMu.RLock()
	defer graphFactoriesMu.RUnlock()
	f, ok := graphFactories[graphType]
	return f, ok
}

// CompileOpts controls graph compilation behavior.
type CompileOpts struct {
	// DurableMember retains required checkpoints for the new frozen member
	// protocol. Legacy graph factories keep their original options.
	DurableMember        bool
	Profile              string
	Effort               contract.EffortLevel
	ToolHooks            []contract.ToolHook     // injected hooks (e.g. SSE tool_result emitter)
	BeforeStepHooks      []loom.StepHook         // injected Before step hooks (e.g. SSE step_start)
	AfterStepHooks       []loom.StepHook         // injected After step hooks (e.g. SSE step_end)
	Store                loom.Store              // for audit logging
	MemoryService        *memory.Service         // if non-nil, enables memory retrieval
	MemoryTopK           int                     // top-K memories to retrieve (default 5)
	MemoryScope          string                  // "tenant" (default) | "user" | "session"
	MemoryReadSources    []memory.RetrieveSource // immutable sources appended after Avatar memory
	AutoRemember         bool                    // if true, auto-extract facts after chat
	MemoryWriteNamespace string                  // optional immutable auto-remember destination
	MemoryWriteSource    string                  // metadata source for an explicit destination
	HookLLM              contract.LLM            // LLM for hooks (e.g. auto-remember); defaults to main LLM
	OnMemoryUpdate       memory.UpdateFunc       // called when auto-remember stores new facts
	// ExecutionLLMWrapper decorates synchronous graph-execution calls. Standard
	// compilation applies it outside fallback; custom factories receive the
	// decorated LLM without gaining fallback behavior.
	ExecutionLLMWrapper func(contract.LLM) contract.LLM

	// Embedder for semantic skill matching (optional; falls back to KeywordMatcher).
	Embedder contract.Embedder

	// Context is runtime key-value context injected into the system prompt
	// as a "## 当前上下文" section. Tools can read it from State["context"].
	Context map[string]any

	// SubAgentStepResolver resolves agent names to host-managed child steps.
	SubAgentStepResolver SubAgentStepResolver

	// AgentRunner executes deterministic worker steps in declarative graphs.
	AgentRunner AgentRunner

	// SkillVersionReader resolves exact SkillVersion bodies for
	// registry_version SkillRefs during live compilation. Nil means those
	// refs cannot be resolved and compilation fails closed (never degrades to
	// an empty body).
	SkillVersionReader SkillVersionReader
}

// CompileAgent converts an AgentRecord into a runnable Loom Graph.
//
// The graph topology adapts to the agent's configuration:
//
//	Simple agent (default):
//	  guard? → prompt_assemble → memory_retrieve? → chat
//
//	Orchestrator agent (sub_agents configured):
//	  guard? → prompt_assemble → memory_retrieve? → chat → route → sub_agent_*...
//
// Each capability is wired only when configured, keeping simple agents simple.
func CompileAgent(tenant string, rec *registry.AgentRecord, llm contract.LLM, tools contract.ToolDispatcher, opts CompileOpts) (*loom.Graph, error) {
	// Work on a defensive copy so resolved skills never mutate the caller's
	// AgentRecord (compilers may be handed shared, cached records).
	recCopy := *rec
	rec = &recCopy

	// Unified live skill resolve — builtins first, then legacy store bodies.
	// Runs before custom factory dispatch so standard and custom graphs share
	// the exact same resolved skill slice and never look up the store twice.
	resolvedSkills, resolveErr := ResolveLiveSkills(
		context.Background(), tenant, rec, opts.Store, opts.SkillVersionReader,
	)
	if resolveErr != nil {
		return nil, resolveErr
	}
	rec.Spec.Skills = resolvedSkills

	// Custom graph factory: if agent has a registered graph_type, delegate entirely.
	if rec.GraphType != "" && rec.GraphType != "standard" {
		if factory, ok := lookupFactory(rec.GraphType); ok {
			return factory(tenant, rec, wrapExecutionLLM(llm, opts), tools, opts)
		}
		slog.Warn("unknown graph_type, falling back to standard compilation",
			"graph_type", rec.GraphType, "agent", rec.Name)
	}

	graphName := tenant + ":" + rec.Name

	effort := opts.Effort
	if effort == "" {
		effort = contract.EffortMedium
	}

	model := rec.Model
	if model == "" {
		model = "deepseek-v4-flash"
	}

	// Wrap LLM with fallback chain if configured.
	activeLLM := llm
	if len(rec.FallbackModels) > 0 {
		activeLLM = llmrouter.NewFallbackLLM(llm, model, rec.FallbackModels, rec.FallbackRetries)
	}
	activeLLM = wrapExecutionLLM(activeLLM, opts)

	// Apply permission wrapping if configured.
	if len(rec.Permissions.Deny) > 0 || len(rec.Permissions.Allow) > 0 || len(rec.Permissions.Ask) > 0 {
		if len(rec.Permissions.Ask) > 0 {
			tools = stdlib.NewPermissionDispatcherWithAsk(tools, rec.Permissions.Deny, rec.Permissions.Ask, rec.Permissions.Allow)
		} else {
			tools = stdlib.NewPermissionDispatcher(tools, rec.Permissions.Deny, rec.Permissions.Allow)
		}
	}

	if rec.ToolLoopControl != nil {
		if err := frozen.ValidateToolLoopControl(rec.ToolLoopControl); err != nil {
			return nil, err
		}
		if !opts.DurableMember || len(rec.SubAgents) > 0 || len(rec.Spec.SubAgents) > 0 || len(rec.Permissions.Ask) > 0 {
			return nil, fmt.Errorf("controlled tool loops require a durable serial leaf without interactive permissions")
		}
	}
	// --- Determine graph topology ---
	hasGuard := rec.Guard != nil && rec.Guard.Enabled
	hasMemory := opts.MemoryService != nil
	resolverAvailable := opts.SubAgentStepResolver != nil
	hasSubAgents := HasSubAgents(rec, resolverAvailable)
	var subAgentRoutes *SubAgentRouteTable
	if hasSubAgents {
		var err error
		subAgentRoutes, err = buildSubAgentRouteTable(rec.SubAgents)
		if err != nil {
			return nil, err
		}
		for _, tool := range rec.Permissions.Ask {
			if strings.EqualFold(strings.TrimSpace(tool), "transfer_to") {
				return nil, fmt.Errorf("agent %q: 本期不支持权限确认移交：permissions.ask 不能包含 transfer_to", rec.Name)
			}
		}
	}

	// Build graph options.
	graphOpts := []loom.GraphOption{
		loom.WithMaxIterations(50),
		loom.WithCheckpointHistory(50),
	}
	if opts.DurableMember {
		graphOpts = append(graphOpts, loom.WithCheckpointPolicy(loom.CheckpointRequired), loom.WithCheckpointHistory(-1))
	}
	if rec.StepBudget > 0 {
		graphOpts = append(graphOpts, loom.WithStepBudget(rec.StepBudget))
	}
	// Use MergeConfig so messages accumulate correctly across sub-graphs.
	if hasSubAgents {
		graphOpts = append(graphOpts, loom.WithMergeConfig(loom.DefaultMergeConfig()), loom.WithCheckpointPolicy(loom.CheckpointRequired))
	}

	// Determine first step.
	firstStep := "prompt_assemble"
	if hasGuard {
		firstStep = "guard"
	}

	g := loom.NewGraph(graphName, firstStep, graphOpts...)

	// Shared live prompt semantics: SystemPrompt → Identity.Core fallback,
	// grounding, always-active/builtin direct injection, matchable subset, and
	// matcher selection. The same preparation drives declarative graphs and
	// the preview endpoint, so every live path agrees on what is injected.
	prepared := PreparePrompt(rec, opts.Embedder)

	// --- Step: Guard (optional) ---
	if hasGuard {
		guardChecks := buildGuardChecks(rec.Guard)
		g.AddStep("guard", stdlib.NewGuardStep(guardChecks...), loom.Condition(
			func(s loom.State) bool {
				blocked, _ := s["__blocked"].(bool)
				return !blocked
			},
			"prompt_assemble", // passed → continue
			"",                // blocked → halt
		))
	}

	// --- Step: Prompt assembly ---
	promptTarget := "chat"
	if hasMemory {
		promptTarget = "memory_retrieve"
	}
	g.AddStep("prompt_assemble", stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
		Identity:              prepared.Identity,
		Skills:                prepared.MatchableSkills, // only skills that need matching (not always_active/builtin)
		Profile:               opts.Profile,
		ProfilesMap:           rec.Spec.Profiles,
		Context:               opts.Context,
		MaxSystemPromptTokens: 8000,
		SkillMatcher:          prepared.Matcher,
	}), loom.Always(promptTarget))

	// --- Step: Memory retrieval (optional) ---
	if hasMemory {
		topK := opts.MemoryTopK
		if topK <= 0 {
			topK = 5
		}
		g.AddStep("memory_retrieve", memory.NewRetrieveStep(memory.RetrieveConfig{
			Service:           opts.MemoryService,
			TopK:              topK,
			Scope:             opts.MemoryScope,
			AdditionalSources: append([]memory.RetrieveSource(nil), opts.MemoryReadSources...),
		}), loom.Always("chat"))
	}

	// --- Step: Chat (ToolLoop) ---
	toolHooks := append([]contract.ToolHook{}, opts.ToolHooks...)
	if rec.MaxToolRepeats > 0 && rec.ToolLoopControl == nil {
		toolHooks = append(toolHooks, newLoopDetector(rec.MaxToolRepeats).hook())
	}

	toolLoopOpts := stdlib.ToolLoopOpts{
		Model:         model,
		SystemPrompt:  rec.Spec.SystemPrompt,
		MaxIterations: 20,
		MaxTokens:     rec.MaxOutputTokens,
		Effort:        effort,
		ToolHooks:     toolHooks,
	}
	if rec.ToolLoopControl != nil {
		toolLoopOpts.MaxToolRepeats = rec.MaxToolRepeats
		toolLoopOpts.MaxIterations = int(rec.ToolLoopControl.SliceRounds)
		toolLoopOpts.Control = &stdlib.ToolLoopControl{ID: "chat", InitialTotalRounds: rec.ToolLoopControl.InitialTotalRounds}
	}
	if hasSubAgents {
		toolLoopOpts.StatePatchPolicy = subAgentRoutes.statePatchPolicy()
	}
	if rec.OutputSchema != nil {
		schema := json.RawMessage(*rec.OutputSchema)
		toolLoopOpts.OutputSchema = &schema
	}

	// Compaction: auto-summarize when context grows too large. Context
	// management defaults to enabled (threshold 6000); explicit disable wins.
	if compaction := registry.EffectiveCompactionConfig(rec); compaction.Enabled {
		threshold := compaction.TokenThreshold
		if threshold <= 0 {
			threshold = 6000
		}
		toolLoopOpts.Compaction = &stdlib.CompactionPolicy{
			Trigger: func(_ []contract.Message, tokenCount int) bool {
				return tokenCount > threshold
			},
			Compactor: buildLLMCompactor(activeLLM, model),
		}
	}

	// Determine chat's next step.
	chatRouter := loom.End()
	if hasSubAgents {
		chatRouter = buildSubAgentRouter(subAgentRoutes)
	}
	g.AddStep("chat", stdlib.NewToolLoopStep(activeLLM, tools, toolLoopOpts), chatRouter)

	// --- Steps: Sub-agent delegation (optional) ---
	if hasSubAgents {
		for _, route := range subAgentRoutes.routes {
			sa := route.Ref
			childStep, err := opts.SubAgentStepResolver(tenant, sa.Name)
			if err != nil {
				return nil, fmt.Errorf("sub-agent %q: %w", sa.Name, err)
			}
			if childStep == nil {
				return nil, fmt.Errorf("sub-agent %q: resolver returned nil step", sa.Name)
			}
			g.AddStep(route.StepName, transferSubAgentStep(route.Canonical, childStep), transferSubGraphRouter)
		}
	}

	// --- Hooks ---
	var beforeHooks []loom.StepHook
	beforeHooks = append(beforeHooks, opts.BeforeStepHooks...)
	if opts.Store != nil {
		beforeHooks = append(beforeHooks, otel.TraceStart(tenant))
	}

	var afterHooks []loom.StepHook
	if opts.Store != nil {
		afterHooks = append(afterHooks, otel.TraceEnd(opts.Store))
		afterHooks = append(afterHooks, otel.AuditHook(opts.Store, tenant, rec.Name))
	}
	memoryHookStart := len(afterHooks)
	if opts.MemoryService != nil && opts.AutoRemember {
		hookLLM := opts.HookLLM
		if hookLLM == nil {
			hookLLM = llm
		}
		if opts.MemoryWriteNamespace != "" {
			afterHooks = append(afterHooks, memory.AutoRememberHookForNamespace(
				hookLLM, opts.MemoryService, model, tenant, rec.Name, opts.MemoryScope,
				opts.MemoryWriteNamespace, opts.MemoryWriteSource, opts.OnMemoryUpdate,
			))
		} else {
			afterHooks = append(afterHooks, memory.AutoRememberHook(
				hookLLM, opts.MemoryService, model, tenant, rec.Name, opts.MemoryScope, opts.OnMemoryUpdate,
			))
		}
	}
	if rec.ToolLoopControl != nil {
		for i, hook := range afterHooks[memoryHookStart:] {
			afterHooks[memoryHookStart+i] = func(ctx context.Context, step string, state loom.State) error {
				if yielded, _ := state["__yield"].(bool); yielded {
					return nil
				}
				return hook(ctx, step, state)
			}
		}
	}
	if rec.MaxCostUSD > 0 {
		afterHooks = append(afterHooks, stdlib.CostBudgetHook(rec.MaxCostUSD))
	}
	if rec.MaxTokens > 0 {
		afterHooks = append(afterHooks, stdlib.TokenBudgetHook(rec.MaxTokens))
	}
	afterHooks = append(afterHooks, opts.AfterStepHooks...)
	if len(beforeHooks) > 0 || len(afterHooks) > 0 {
		g.SetHooks(loom.HookPoints{Before: beforeHooks, After: afterHooks})
	}

	// Declare topology for visualization.
	var topo []loom.StepInfo
	if hasGuard {
		topo = append(topo, loom.StepInfo{Name: "guard", Detail: "input validation", Edges: []loom.Edge{{To: "prompt_assemble", Label: "pass"}, {To: "", Label: "blocked"}}})
	}
	topo = append(topo, loom.StepInfo{Name: "prompt_assemble", Detail: fmt.Sprintf("%d skills", len(rec.Spec.Skills)), Edges: []loom.Edge{{To: promptTarget}}})
	if hasMemory {
		topo = append(topo, loom.StepInfo{Name: "memory_retrieve", Detail: fmt.Sprintf("top_k=%d", opts.MemoryTopK), Edges: []loom.Edge{{To: "chat"}}})
	}
	chatNext := ""
	if hasSubAgents {
		chatNext = "router"
	}
	topo = append(topo, loom.StepInfo{Name: "chat", Detail: model, Edges: []loom.Edge{{To: chatNext}}})
	if hasSubAgents {
		var branches []loom.Edge
		for _, route := range subAgentRoutes.routes {
			branches = append(branches, loom.Edge{To: route.StepName, Label: route.Canonical})
		}
		branches = append(branches, loom.Edge{To: "", Label: "no delegation"})
		topo = append(topo, loom.StepInfo{Name: "router", Edges: branches})
		for _, route := range subAgentRoutes.routes {
			topo = append(topo, loom.StepInfo{Name: route.StepName, Detail: route.Ref.Name, Edges: []loom.Edge{{To: ""}}})
		}
	}
	g.SetTopology(topo)

	return g, nil
}

func wrapExecutionLLM(llm contract.LLM, opts CompileOpts) contract.LLM {
	if opts.ExecutionLLMWrapper == nil {
		return llm
	}
	return opts.ExecutionLLMWrapper(llm)
}

// ---------------------------------------------------------------------------
// Guard checks
// ---------------------------------------------------------------------------

// BuildGuardStep creates a Guard step from GuardConfig. Exported for declarative graph factory.
func BuildGuardStep(cfg *registry.GuardConfig) loom.Step {
	return stdlib.NewGuardStep(buildGuardChecks(cfg)...)
}

// buildGuardChecks creates StepHook checks from GuardConfig.
func buildGuardChecks(cfg *registry.GuardConfig) []stdlib.StepHook {
	var checks []stdlib.StepHook

	if cfg.MaxInputLen > 0 {
		maxLen := cfg.MaxInputLen
		checks = append(checks, func(_ context.Context, _ string, state loom.State) error {
			msg, _ := state["last_user_message"].(string)
			if len(msg) > maxLen {
				return fmt.Errorf("input too long: %d chars (max %d)", len(msg), maxLen)
			}
			return nil
		})
	}

	if len(cfg.BlockedTerms) > 0 {
		terms := cfg.BlockedTerms
		checks = append(checks, func(_ context.Context, _ string, state loom.State) error {
			msg, _ := state["last_user_message"].(string)
			lower := strings.ToLower(msg)
			for _, term := range terms {
				if strings.Contains(lower, strings.ToLower(term)) {
					return fmt.Errorf("input contains blocked term")
				}
			}
			return nil
		})
	}

	return checks
}

// ---------------------------------------------------------------------------
// Compaction
// ---------------------------------------------------------------------------

// buildLLMCompactor creates a Compactor that uses the LLM to summarize old messages.
func buildLLMCompactor(llm contract.LLM, model string) func(ctx context.Context, msgs []contract.Message) ([]contract.Message, error) {
	return func(ctx context.Context, msgs []contract.Message) ([]contract.Message, error) {
		if len(msgs) <= 4 {
			return msgs, nil // too few to compact
		}

		// Keep system prompt + last 2 messages; summarize the middle.
		var systemMsg *contract.Message
		start := 0
		if msgs[0].Role == "system" {
			systemMsg = &msgs[0]
			start = 1
		}

		keepLast := 2
		if len(msgs)-start <= keepLast {
			return msgs, nil
		}

		toSummarize := msgs[start : len(msgs)-keepLast]
		tail := msgs[len(msgs)-keepLast:]

		// Build summary prompt.
		var sb strings.Builder
		for _, m := range toSummarize {
			sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
		}

		resp, err := llm.Chat(ctx, contract.ChatRequest{
			Model: model,
			Messages: []contract.Message{
				{Role: "system", Content: "Summarize the following conversation into a concise paragraph. Preserve key facts, decisions, and context. Output only the summary."},
				{Role: "user", Content: sb.String()},
			},
		})
		if err != nil {
			return msgs, nil // compaction failed, keep originals
		}

		// Rebuild: system + summary + tail.
		var result []contract.Message
		if systemMsg != nil {
			result = append(result, *systemMsg)
		}
		result = append(result, contract.Message{
			Role:    "assistant",
			Content: "[Previous conversation summary]\n" + resp.Content,
		})
		result = append(result, tail...)
		return result, nil
	}
}

// ---------------------------------------------------------------------------
// Sub-agent routing
// ---------------------------------------------------------------------------

// buildSubAgentRouter creates a Router that inspects chat output for delegation hints.
// The LLM can set __delegate_to in its output to route to a sub-agent,
// or the router checks the output for route_key matches.
func buildSubAgentRouter(table *SubAgentRouteTable) loom.Router {
	return func(_ context.Context, s loom.State) (string, error) {
		delegate, _ := s["__delegate_to"].(string)
		if delegate == "" {
			return "", nil
		}
		if next, ok := table.byCanonical[delegate]; ok {
			return next.StepName, nil
		}
		return "", fmt.Errorf("transfer protocol error: unknown __delegate_to %q; available canonical keys: %s", delegate, strings.Join(table.CanonicalKeys(), ", "))
	}
}

// HasSubAgents is the single topology predicate shared by compiler and API tool assembly.
func HasSubAgents(rec *registry.AgentRecord, resolverAvailable bool) bool {
	return rec != nil && len(rec.SubAgents) > 0 && resolverAvailable
}

type SubAgentRoute struct {
	Ref                 registry.SubAgentRef
	Canonical, StepName string
}
type SubAgentRouteTable struct {
	routes      []SubAgentRoute
	byAlias     map[string]SubAgentRoute
	byCanonical map[string]SubAgentRoute
}

func BuildSubAgentRouteTable(refs []registry.SubAgentRef) (*SubAgentRouteTable, error) {
	return buildSubAgentRouteTable(refs)
}
func buildSubAgentRouteTable(refs []registry.SubAgentRef) (*SubAgentRouteTable, error) {
	t := &SubAgentRouteTable{byAlias: map[string]SubAgentRoute{}, byCanonical: map[string]SubAgentRoute{}}
	claimed := map[string]string{}
	claim := func(value, owner string) error {
		if value == "" || strings.TrimSpace(value) != value {
			return fmt.Errorf("sub-agent route %q is blank or has surrounding whitespace", value)
		}
		fold := strings.ToLower(value)
		if prior, ok := claimed[fold]; ok {
			return fmt.Errorf("sub-agent route collision %q between %s and %s", value, prior, owner)
		}
		claimed[fold] = owner
		return nil
	}
	for _, ref := range refs {
		canonical := ref.RouteKey
		if canonical == "" {
			canonical = ref.Name
		}
		step := "sub_" + ref.Name
		if strings.EqualFold(canonical, step) {
			return nil, fmt.Errorf("sub-agent canonical key %q collides with step name %q", canonical, step)
		}
		local := map[string]bool{}
		for _, value := range []string{ref.Name, canonical, step} {
			fold := strings.ToLower(value)
			if local[fold] {
				continue
			}
			local[fold] = true
			if err := claim(value, ref.Name); err != nil {
				return nil, err
			}
		}
		r := SubAgentRoute{Ref: ref, Canonical: canonical, StepName: step}
		t.routes = append(t.routes, r)
		t.byAlias[ref.Name] = r
		t.byAlias[canonical] = r
		t.byCanonical[canonical] = r
	}
	return t, nil
}
func (t *SubAgentRouteTable) Routes() []SubAgentRoute {
	if t == nil {
		return nil
	}
	return append([]SubAgentRoute(nil), t.routes...)
}
func (t *SubAgentRouteTable) Resolve(alias string) (SubAgentRoute, bool) {
	if t == nil {
		return SubAgentRoute{}, false
	}
	r, ok := t.byAlias[alias]
	return r, ok
}
func (t *SubAgentRouteTable) CanonicalKeys() []string {
	keys := make([]string, 0, len(t.byCanonical))
	for k := range t.byCanonical {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func (t *SubAgentRouteTable) statePatchPolicy() *stdlib.StatePatchPolicy {
	return &stdlib.StatePatchPolicy{Allowed: map[string]map[string]func(any) error{"transfer_to": {"__delegate_to": func(value any) error {
		s, ok := value.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		if _, ok := t.byCanonical[s]; !ok {
			return fmt.Errorf("%q is not a canonical key", s)
		}
		return nil
	}}}}
}
func transferSubAgentStep(target string, inner loom.Step) loom.Step {
	return func(ctx context.Context, state loom.State) (loom.State, error) {
		update, err := inner(ctx, state)
		if err != nil {
			return loom.State{
				"__delegate_failed": target,
				"__delegate_error":  err.Error(),
				"__deleted_keys":    []string{"__delegate_to"},
				"output":            "移交失败/待处理",
			}, nil
		}
		return withDeletedStateKey(update, "__delegate_to"), nil
	}
}

func transferSubGraphRouter(_ context.Context, state loom.State) (string, error) {
	if message, failed := state["__delegate_error"].(string); failed && message != "" {
		return "", fmt.Errorf("%s", message)
	}
	return "", nil
}

func withDeletedStateKey(update loom.State, key string) loom.State {
	result := make(loom.State, len(update)+1)
	for name, value := range update {
		result[name] = value
	}
	deleted, _ := update["__deleted_keys"].([]string)
	result["__deleted_keys"] = append(append([]string(nil), deleted...), key)
	return result
}

// ---------------------------------------------------------------------------
// Builtin skills
// ---------------------------------------------------------------------------

// BuiltinSkills are pre-defined skills that can be referenced by name.
// When an agent's skill list includes a matching name, the body is
// automatically populated from here.
var BuiltinSkills = map[string]stdlib.SkillDef{
	"rich-output": {
		Name:        "rich-output",
		Description: "Render chart table diagram data visualization graph plot output display result",
		Body: `The UI renders structured content blocks natively. Use these formats instead of describing data in prose.

IMPORTANT: Generate data directly. Do NOT call tools or search — create reasonable example/synthetic data.

## Charts
` + "```chart" + `
{"chart_type":"bar","title":"Monthly sales","data":{"labels":["Jan","Feb","Mar"],"datasets":[{"label":"Revenue","values":[120,200,150]}]}}
` + "```" + `
Rules:
- chart_type: bar | line | pie | area | scatter
- data.labels: x-axis labels array
- data.datasets: array of {label, values} — values is number[]
- Use "values" NOT "data". No options/scales/plugins.

## Mermaid diagrams
For architecture, flow, sequence, or concept diagrams:
` + "```mermaid" + `
graph TD
    A[Start] --> B{Decision}
    B -->|Yes| C[Action]
    B -->|No| D[End]
` + "```" + `
Supports: graph, sequenceDiagram, classDiagram, stateDiagram, erDiagram, gantt, pie, flowchart.

## SVG graphics
For custom visuals, architecture diagrams, or detailed illustrations:
` + "```svg" + `
<svg viewBox="0 0 400 200" xmlns="http://www.w3.org/2000/svg">
  <rect x="10" y="10" width="120" height="60" rx="8" fill="#F5F0EB" stroke="#D4714A"/>
  <text x="70" y="45" text-anchor="middle" font-size="14" fill="#1A1714">Component</text>
</svg>
` + "```" + `
Use warm palette: #D4714A (terracotta), #2D6A2D (sage), #1A56A0 (woad), #F5F0EB (linen), #1A1714 (ink).

## Tables
Use markdown tables with | separators.

Always prefer structured output over prose. Combine multiple block types in one response.`,
	},
	"structured-ui": {
		Name:        "structured-ui",
		Description: "structured UI component output: data card status bar alert action button quick options table chart",
		Body: `你的回复会被渲染成结构化 UI 组件。请使用以下格式输出组件，而不是纯文本描述。

## 组件输出格式

用 ` + "```component" + ` 围栏块输出 JSON，每个块是一个 UI 组件。普通文本段落直接用 Markdown 输出即可。

### 可用组件

**data-card** — 数据卡片（标题 + 指标 + 状态标签）
` + "```component" + `
{"type":"data-card","title":"概览","metrics":[{"label":"总数","value":"128","big":true},{"label":"分类","value":"12"}],"tags":[{"text":"正常","signal":"safe"}]}
` + "```" + `
- metrics: 数组，每项 {label, value, big?}
- tags: 数组，每项 {text, signal}，signal: safe | warn | danger | stale | info

**status-bar** — 水位条
` + "```component" + `
{"type":"status-bar","label":"指标A","current":2,"safety":5,"unit":"个"}
` + "```" + `
- current/safety 为数字，自动计算比例和颜色

**alert-banner** — 提醒横幅
` + "```component" + `
{"type":"alert-banner","signal":"warn","text":"检测到异常情况，请及时关注"}
` + "```" + `

**action-buttons** — 操作按钮组
` + "```component" + `
{"type":"action-buttons","buttons":[{"label":"批准","variant":"primary","action":"批准此项"},{"label":"驳回","variant":"secondary","action":"驳回此项"},{"label":"详情","variant":"ghost","action":"查看详细信息"}]}
` + "```" + `
- variant: primary | secondary | ghost
- action: 用户点击后发送给你的消息文本

**quick-options** — 快捷选项（胶囊按钮）
` + "```component" + `
{"type":"quick-options","options":[{"label":"查看详情","value":"查看详情"},{"label":"导出报告","value":"导出报告"},{"label":"今日记录","value":"查看今日记录"}]}
` + "```" + `
- value: 用户点击后发送给你的消息文本

**table** — 数据表格
` + "```component" + `
{"type":"table","title":"明细","columns":[{"key":"name","label":"名称"},{"key":"qty","label":"数量","align":"right"},{"key":"value","label":"金额","align":"right"}],"rows":[{"name":"项目A","qty":"2","value":"16.4万"},{"name":"项目B","qty":"8","value":"6.4万"}]}
` + "```" + `
- columns: 数组，每项 {key, label, align?}，align: left(默认) | center | right
- rows: 数组，每项是 {[key]: value} 对象

**bar-chart** — 柱状图
` + "```component" + `
{"type":"bar-chart","title":"分布","data":[{"name":"类别A","amount":79.8},{"name":"类别B","amount":4.2}],"xKey":"name","series":[{"key":"amount","label":"数量"}]}
` + "```" + `
- data: 数据数组，每项是一个对象
- xKey: X轴对应的字段名
- series: 数据系列数组，每项 {key, label, color?}；多个系列会并排显示

**line-chart** — 折线图
` + "```component" + `
{"type":"line-chart","title":"趋势","data":[{"month":"1月","actual":85,"target":56},{"month":"2月","actual":82,"target":56}],"xKey":"month","series":[{"key":"actual","label":"实际值"},{"key":"target","label":"目标值"}]}
` + "```" + `
- 与 bar-chart 格式一致，适合展示趋势数据

## 输出规则

1. **混合使用**：Markdown 文本和组件可以交替出现，构成自然的叙事流
2. **数据卡片优先**：有数字指标时用 data-card，不要用列表
3. **操作可点击**：需要用户决策时用 action-buttons，提供清晰的操作选项
4. **结尾快捷选项**：叙事结尾适当提供 quick-options 引导下一步
5. **状态可视化**：涉及阈值对比时用 status-bar
6. **表格适用于明细**：多行结构化数据用 table，不要用 Markdown 表格
7. **图表适用于趋势和对比**：分布/对比用 bar-chart，趋势变化用 line-chart
8. **JSON 必须合法**：确保 JSON 格式正确，不要有尾逗号
9. **所有文本用中文**`,
	},
}

// ResolveSkillsFromStore loads skill bodies from the store for any skill with empty body.
// This allows agents to reference skills by name without copying the body.
func ResolveSkillsFromStore(skills []stdlib.SkillDef, store loom.Store, tenant string) []stdlib.SkillDef {
	out := make([]stdlib.SkillDef, len(skills))
	for i, s := range skills {
		// Always try to load from skill library to get latest body + always_active flag.
		data, err := store.Get(context.Background(), "skill:"+tenant, s.Name)
		if err == nil && data != nil {
			var stored struct {
				Body         string `json:"body"`
				Description  string `json:"description"`
				AlwaysActive bool   `json:"always_active"`
			}
			if json.Unmarshal(data, &stored) == nil {
				if stored.Body != "" {
					s.Body = stored.Body
				}
				if stored.Description != "" && s.Description == "" {
					s.Description = stored.Description
				}
				s.AlwaysActive = stored.AlwaysActive
			}
		}
		out[i] = s
	}
	return out
}

// ResolveLiveSkills is the single live-path skill resolver shared by the
// standard compiler, custom graph factories (declarative), and the preview
// endpoint. Explicit SkillRefs are resolved first in record order:
// registry_version refs are pinned through the SkillVersionReader to their
// exact body (fail-closed on a missing reader, missing version, or
// cross-workspace ref), while legacy/builtin refs follow the D2 path (builtin
// bodies, then legacy store lookup in namespace "skill:<tenant>"). Remaining
// Spec.Skills then follow the same D2 path. Pinned registry_version bodies
// never pass through the legacy store lookup, so a later version can never
// overwrite the exact pinned body. store may be nil; reader may be nil only
// when the record carries no registry_version refs.
func ResolveLiveSkills(
	ctx context.Context,
	tenant string,
	rec *registry.AgentRecord,
	store loom.Store,
	reader SkillVersionReader,
) ([]stdlib.SkillDef, error) {
	// 1. Resolve explicit SkillRefs in record order. registry_version refs are
	// pinned to an exact immutable version and must not be re-resolved by the
	// legacy store lookup below; legacy/builtin refs still need the D2 pass.
	refDefs := make([]stdlib.SkillDef, 0, len(rec.SkillRefs))
	refPinned := make([]bool, 0, len(rec.SkillRefs))
	d2Input := make([]stdlib.SkillDef, 0, len(rec.SkillRefs)+len(rec.Spec.Skills))
	for i := range rec.SkillRefs {
		ref := &rec.SkillRefs[i]
		switch ref.SourceType {
		case registry.SourceTypeRegistryVersion:
			if reader == nil {
				return nil, fmt.Errorf(
					"agent %q: skill version reader is unavailable; cannot resolve registry_version skill %q",
					rec.Name, ref.SkillID,
				)
			}
			if ref.SkillID == "" || ref.SkillVersion == nil || *ref.SkillVersion < 1 {
				return nil, fmt.Errorf(
					"%w: agent %q skill %q must pin an exact positive skill_version",
					registry.ErrSkillVersionRequired, rec.Name, ref.SkillID,
				)
			}
			version, getErr := reader.GetVersion(ctx, tenant, ref.SkillID, *ref.SkillVersion)
			if getErr != nil {
				return nil, fmt.Errorf(
					"agent %q: resolve skill version %q@%d: %w",
					rec.Name, ref.SkillID, *ref.SkillVersion, getErr,
				)
			}
			if version == nil {
				return nil, fmt.Errorf(
					"agent %q: skill version %q@%d does not exist",
					rec.Name, ref.SkillID, *ref.SkillVersion,
				)
			}
			if version.WorkspaceID != tenant {
				return nil, fmt.Errorf(
					"agent %q: skill version %q@%d belongs to workspace %q, not %q",
					rec.Name, ref.SkillID, *ref.SkillVersion, version.WorkspaceID, tenant,
				)
			}
			def := stdlib.SkillDef{
				Name:         ref.Name,
				Description:  ref.Description,
				Body:         version.Body,
				AlwaysActive: version.AlwaysActive,
			}
			if def.Description == "" {
				def.Description = version.Description
			}
			refDefs = append(refDefs, def)
			refPinned = append(refPinned, true)
		case registry.SourceTypeLegacy, registry.SourceTypeBuiltin:
			refDefs = append(refDefs, stdlib.SkillDef{
				Name:        ref.Name,
				Description: ref.Description,
			})
			refPinned = append(refPinned, false)
			d2Input = append(d2Input, refDefs[len(refDefs)-1])
		default:
			return nil, fmt.Errorf(
				"agent %q: unknown skill source_type %q",
				rec.Name, ref.SourceType,
			)
		}
	}

	// 2. D2 path for legacy/builtin refs plus the record's spec skills.
	d2Input = append(d2Input, rec.Spec.Skills...)
	d2Resolved := ResolveBuiltinSkills(d2Input)
	if store != nil {
		d2Resolved = ResolveSkillsFromStore(d2Resolved, store, tenant)
	}

	// 3. Merge back in record order: refs first (pinned bodies in place of
	// their legacy-path siblings), then spec skills — deterministic and
	// stable regardless of source-type mixing.
	out := make([]stdlib.SkillDef, 0, len(refDefs)+len(rec.Spec.Skills))
	d2Index := 0
	for i := range refDefs {
		if refPinned[i] {
			out = append(out, refDefs[i])
		} else {
			out = append(out, d2Resolved[d2Index])
			d2Index++
		}
	}
	out = append(out, d2Resolved[d2Index:]...)
	return out, nil
}

// PreparedPrompt carries the shared live prompt preparation result: the
// grounded identity with always-active/builtin skills injected, the matchable
// skill subset, and the matcher selected for this context.
type PreparedPrompt struct {
	Identity        stdlib.IdentitySpec
	MatchableSkills []stdlib.SkillDef
	Matcher         stdlib.SkillMatcher
}

// PreparePrompt applies the shared live prompt semantics used by standard
// compilation, custom (declarative) factories, and the preview endpoint:
//
//   - SystemPrompt (v1.2 compat) falls back into Identity.Core;
//   - the platform grounding rule is attached once;
//   - always-active/builtin skills are injected directly into identity.Core;
//   - remaining skills form the matchable subset (index always shown, body
//     disclosed only on match);
//   - SemanticMatcher is used when an embedder is available, otherwise
//     KeywordMatcher.
//
// Callers must pass a record whose Spec.Skills are already resolved via
// ResolveLiveSkills.
func PreparePrompt(rec *registry.AgentRecord, embedder contract.Embedder) PreparedPrompt {
	identity := rec.Spec.Identity
	if identity.Core == "" && rec.Spec.SystemPrompt != "" {
		identity.Core = rec.Spec.SystemPrompt
	}
	// Platform grounding rule (anti-fabrication) — one shared source of truth
	// (grounding.GroundIdentity) across the standard, declarative, and
	// CLI-engine identity paths, so no agent class silently misses it.
	identity.Core = grounding.GroundIdentity(identity.Core)
	var matchableSkills []stdlib.SkillDef
	for _, s := range rec.Spec.Skills {
		if s.AlwaysActive && s.Body != "" {
			// Always-active: inject directly, skip matcher.
			identity.Core += "\n\n## 技能: " + s.Name + "\n以下规则必须遵守：\n" + s.Body
		} else if builtin, ok := BuiltinSkills[s.Name]; ok && builtin.Body != "" {
			// Builtin: inject directly (backward compat).
			identity.Core += "\n\n## " + s.Name + "\n" + builtin.Body
		} else {
			// Regular: progressive disclosure via the selected matcher.
			matchableSkills = append(matchableSkills, s)
		}
	}
	var matcher stdlib.SkillMatcher
	if embedder != nil {
		matcher = NewSemanticMatcher(embedder)
	} else {
		matcher = &stdlib.KeywordMatcher{}
	}
	return PreparedPrompt{
		Identity:        identity,
		MatchableSkills: matchableSkills,
		Matcher:         matcher,
	}
}

// ResolveBuiltinSkills fills in the body of any skills that match a builtin name.
func ResolveBuiltinSkills(skills []stdlib.SkillDef) []stdlib.SkillDef {
	out := make([]stdlib.SkillDef, len(skills))
	for i, s := range skills {
		if builtin, ok := BuiltinSkills[s.Name]; ok && s.Body == "" {
			s.Body = builtin.Body
			if s.Description == "" {
				s.Description = builtin.Description
			}
		}
		out[i] = s
	}
	return out
}

// NoOpDispatcher is a tool dispatcher that provides no tools.
type NoOpDispatcher struct{}

func (d *NoOpDispatcher) ListTools(_ context.Context) ([]contract.ToolDef, error) {
	return nil, nil
}

func (d *NoOpDispatcher) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	return &contract.ToolResult{
		CallID:  call.ID,
		Content: "no tools configured",
		IsError: true,
	}, nil
}
