package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/fileartifact"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execspec"
	"github.com/jinyitao123/weave/internal/kernel/freezer"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimellm"
)

type RuntimeCredentialResolver interface {
	Validate(context.Context, frozen.CredentialReference) error
	Resolve(context.Context, credentials.ResolveRequest) (credentials.SecretMaterial, error)
}

type RuntimeHostFactory interface {
	Build(
		context.Context,
		frozen.FrozenExecutionBundle,
		RuntimeCredentialResolver,
	) (compiler.FrozenBuildOpts, io.Closer, error)
}

type RuntimeCLIExecutor interface {
	ExecRemote(
		context.Context,
		string,
		*registry.AgentRecord,
		execution.AgentExecutionStamp,
		string,
		[]execspec.Attachment,
	) (engine.RunResult, error)
}

type RuntimeCLIEntry struct {
	executor  RuntimeCLIExecutor
	record    *registry.AgentRecord
	stamp     execution.AgentExecutionStamp
	frozenMCP *execspec.FrozenMCPInvocation
}

// RuntimeCLIUsageAttempt is the TeamRun-facing, lossless subset of one engine
// attempt. A nil engine receipt is represented by both dimensions false, not
// by an estimated zero.
type RuntimeCLIUsageAttempt struct {
	AttemptID    string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	HasTokens    bool
	HasCost      bool
	Source       string
	ToolCalls    int
	Events       []RuntimeCLIEvent
}

// RuntimeCLIEvent is the TeamRun-facing subset of one observed engine event.
type RuntimeCLIEvent struct {
	Kind   string `json:"kind"`
	Tool   string `json:"tool,omitempty"`
	CallID string `json:"call_id,omitempty"`
	Status string `json:"status,omitempty"`
	Input  string `json:"input,omitempty"`
	Output string `json:"output,omitempty"`
}

// RuntimeCLIArtifact is the TeamRun-facing subset of one bounded delivery file.
type RuntimeCLIArtifact struct {
	Path, ContentType, Content string
}

type RuntimeCLIResult struct {
	Status, Err, SessionID string
	Diagnostics            []engine.Diagnostic
	ArtifactCollection     *fileartifact.CollectionEvidence
	Output                 string
	Attempts               []RuntimeCLIUsageAttempt
	Events                 []RuntimeCLIEvent
	Artifacts              []RuntimeCLIArtifact
	// DeliveryError is retained for old checkpoint compatibility. New collection
	// observations travel in ArtifactCollection and never become engine errors.
	DeliveryError string
}

// TextOutput resolves a single explicitly referenced delivery file to its
// collected content. Structured outputs and ambiguous multi-file answers remain
// separate; no host path is opened by the workflow process.
func (r RuntimeCLIResult) TextOutput() (string, error) {
	var selected *RuntimeCLIArtifact
	for index := range r.Artifacts {
		artifact := &r.Artifacts[index]
		if !engine.ReferencesArtifact(r.Output, artifact.Path) && !engine.ReferencesArtifact(r.Output, "outputs/"+artifact.Path) {
			continue
		}
		if selected != nil {
			return r.Output, nil
		}
		selected = artifact
	}
	if selected == nil {
		return r.Output, nil
	}
	if strings.TrimSpace(selected.Content) == "" {
		return r.Output, nil
	}
	return selected.Content, nil
}

// ObservedEvents returns the bounded physical-attempt events when available.
// An aggregate event list is only authoritative for executors without attempts.
func (r RuntimeCLIResult) ObservedEvents(limit int) []RuntimeCLIEvent {
	if limit <= 0 {
		return nil
	}
	events := make([]RuntimeCLIEvent, 0, min(limit, len(r.Events)))
	if len(r.Attempts) == 0 {
		return append(events, r.Events[:min(limit, len(r.Events))]...)
	}
	for _, attempt := range r.Attempts {
		for _, event := range attempt.Events {
			if len(events) >= limit {
				return events
			}
			if attempt.AttemptID != "" && event.CallID != "" {
				event.CallID = attempt.AttemptID + ":" + event.CallID
			}
			events = append(events, event)
		}
	}
	if len(events) > 0 {
		return events
	}
	return append(events, r.Events[:min(limit, len(r.Events))]...)
}

func NewRuntimeCLIEntry(
	executor RuntimeCLIExecutor,
	record *registry.AgentRecord,
	stamp execution.AgentExecutionStamp,
) (*RuntimeCLIEntry, error) {
	if runtimeNilLike(executor) || record == nil {
		return nil, runtimeHostUnsupportedError("CLI runtime executor is unavailable")
	}
	return &RuntimeCLIEntry{executor: executor, record: record, stamp: stamp}, nil
}

func (e *RuntimeCLIEntry) Execute(ctx context.Context, prompt string) (string, error) {
	result, err := e.ExecuteResult(ctx, prompt)
	return result.Output, err
}

// ExecuteAccounted exposes every physical attempt to TeamRun accounting. The
// result is returned even when execution fails so already incurred spend can
// be committed before the failure route is chosen.
func (e *RuntimeCLIEntry) ExecuteAccounted(ctx context.Context, prompt string) (RuntimeCLIResult, error) {
	result, err := e.ExecuteResult(ctx, prompt)
	accounted := RuntimeCLIResult{Output: result.Output, Status: result.Status, Err: result.Err, SessionID: result.SessionID,
		Diagnostics: append([]engine.Diagnostic(nil), result.Diagnostics...), ArtifactCollection: result.ArtifactCollection}
	err = errors.Join(err, fileartifact.CollectionError(result.ArtifactCollection))
	if len(result.Attempts) > 0 {
		accounted.Attempts = make([]RuntimeCLIUsageAttempt, 0, len(result.Attempts))
		for _, attempt := range result.Attempts {
			accounted.Attempts = append(accounted.Attempts, runtimeCLIUsageAttempt(attempt.AttemptID, attempt.Usage, attempt.Events))
		}
	} else {
		accounted.Attempts = []RuntimeCLIUsageAttempt{runtimeCLIUsageAttempt("", result.Usage, result.Events)}
	}
	for _, event := range result.Events {
		accounted.Events = append(accounted.Events, runtimeCLIEvent(event))
	}
	for _, artifact := range result.Artifacts {
		accounted.Artifacts = append(accounted.Artifacts, RuntimeCLIArtifact{
			Path: artifact.Path, ContentType: artifact.ContentType, Content: artifact.Content,
		})
	}
	return accounted, err
}

func runtimeCLIUsageAttempt(attemptID string, receipt *engine.UsageReceipt, events []engine.Event) RuntimeCLIUsageAttempt {
	usage := RuntimeCLIUsageAttempt{AttemptID: attemptID}
	for _, event := range events {
		usage.Events = append(usage.Events, runtimeCLIEvent(event))
	}
	seen := make(map[string]struct{})
	for _, event := range events {
		if event.Kind != "tool_call" && event.Kind != "tool_result" {
			continue
		}
		key := event.CallID
		if key == "" {
			key = event.Tool
		}
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			usage.ToolCalls++
		}
	}
	if receipt == nil {
		return usage
	}
	usage.InputTokens = receipt.InputTokens
	usage.OutputTokens = receipt.OutputTokens
	usage.CostUSD = receipt.CostUSD
	usage.HasTokens = receipt.HasTokens
	usage.HasCost = receipt.HasCost
	usage.Source = receipt.Source
	return usage
}

func runtimeCLIEvent(event engine.Event) RuntimeCLIEvent {
	return RuntimeCLIEvent{Kind: event.Kind, Tool: event.Tool, CallID: event.CallID, Status: event.Status, Input: event.Input, Output: event.Output}
}

// ExecuteResult preserves the CLI receipt for TeamRun node accounting.
func (e *RuntimeCLIEntry) ExecuteResult(ctx context.Context, prompt string) (engine.RunResult, error) {
	if e == nil || runtimeNilLike(e.executor) || e.record == nil {
		return engine.RunResult{}, runtimeHostUnsupportedError("CLI runtime executor is unavailable")
	}
	if e.frozenMCP != nil {
		ctx = execspec.WithFrozenMCPInvocation(ctx, *e.frozenMCP)
	}
	return e.executor.ExecRemote(
		ctx,
		e.record.WorkspaceID,
		e.record,
		e.stamp,
		prompt,
		nil,
	)
}

type RuntimeGraphEntry struct {
	AgentID      string
	AgentVersion int64
	Graph        *loom.Graph
	CLI          *RuntimeCLIEntry
	Bundle       *frozen.FrozenExecutionBundle
}

type RuntimeArtifact struct {
	WorkspaceID     string
	WorkflowID      string
	WorkflowVersion int
	Entries         []RuntimeGraphEntry
	DeliveryTargets []frozen.FrozenDeliveryTarget

	closers []io.Closer
	closed  bool
}

func (a *RuntimeArtifact) Close() error {
	if a == nil || a.closed {
		return nil
	}
	a.closed = true

	closeErrors := make([]error, 0, len(a.closers))
	for index := len(a.closers) - 1; index >= 0; index-- {
		if err := a.closers[index].Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

type RuntimeLoader struct {
	Registry      *compiler.DescriptorRegistry
	CLIExecutor   RuntimeCLIExecutor
	RunSnapshotID string
}

func (l *RuntimeLoader) Load(
	ctx context.Context,
	envelope frozen.ArtifactEnvelopeV1,
	factory RuntimeHostFactory,
	credentialResolver RuntimeCredentialResolver,
) (*RuntimeArtifact, error) {
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return nil, err
	}
	ctx = withPublishedServiceCredentialAuthorization(ctx, payload)

	if len(payload.DeliveryTargets) > 0 {
		if runtimeNilLike(credentialResolver) {
			return nil, credentials.ErrCredentialUnavailable
		}
		for _, target := range payload.DeliveryTargets {
			if err := credentialResolver.Validate(ctx, target.AccessRef); err != nil {
				if runtimeNilLike(err) {
					return nil, credentials.ErrCredentialUnavailable
				}
				return nil, err
			}
			for _, binding := range target.CredentialBindings {
				if err := credentialResolver.Validate(ctx, binding.CredentialRef); err != nil {
					if runtimeNilLike(err) {
						return nil, credentials.ErrCredentialUnavailable
					}
					return nil, err
				}
			}
		}
	}
	runtimeLLMDefault, runtimeLLMDefaultErr := runtimeDefaultForProviderlessLoom(payload.Bundles)

	artifact := &RuntimeArtifact{
		WorkspaceID:     envelope.WorkspaceID,
		WorkflowID:      envelope.WorkflowID,
		WorkflowVersion: envelope.WorkflowVersion,
		Entries:         make([]RuntimeGraphEntry, 0, len(payload.Bundles)),
		DeliveryTargets: payload.DeliveryTargets,
		closers:         make([]io.Closer, 0, len(payload.Bundles)),
	}
	cleanup := func() {
		for index := len(artifact.closers) - 1; index >= 0; index-- {
			_ = artifact.closers[index].Close()
		}
	}

	for _, bundle := range payload.Bundles {
		resolver, rebuildErr := freezer.RebuildArtifactResolver(bundle)
		if rebuildErr != nil {
			cleanup()
			return nil, rebuildErr
		}
		if bundle.Runtime != nil && bundle.Runtime.Engine != "loom" {
			if !engine.IsCLIEngine(bundle.Runtime.Engine) {
				cleanup()
				return nil, runtimeHostUnsupportedError("CLI runtime engine is not supported")
			}
			if runtimeNilLike(l.CLIExecutor) {
				cleanup()
				return nil, runtimeHostUnsupportedError("CLI runtime executor is unavailable")
			}
			if runtimeNilLike(credentialResolver) {
				cleanup()
				return nil, credentials.ErrCredentialUnavailable
			}
			if err := credentialResolver.Validate(ctx, bundle.Runtime.AccessRef); err != nil {
				cleanup()
				if runtimeNilLike(err) {
					return nil, credentials.ErrCredentialUnavailable
				}
				return nil, err
			}
			if err := compiler.ValidateStandardMCPBindings(bundle); err != nil {
				cleanup()
				return nil, err
			}
			record := runtimeCLIAgentRecord(bundle)
			cliEntry, entryErr := NewRuntimeCLIEntry(
				l.CLIExecutor,
				record,
				execution.AgentExecutionStamp{
					AgentID:        bundle.Agent.AgentID,
					AgentVersion:   int(bundle.Agent.AgentVersion),
					ExecutionScope: execution.ScopeTeamWorkerLeaf,
					RunSnapshotID:  l.RunSnapshotID,
				},
			)
			if entryErr != nil {
				cleanup()
				return nil, entryErr
			}
			if bundle.FactoryKey == compiler.StandardFrozenCLIToolsKey() {
				cliEntry.frozenMCP = &execspec.FrozenMCPInvocation{WorkspaceID: bundle.Agent.WorkspaceID, AgentID: bundle.Agent.AgentID, AgentVersion: bundle.Agent.AgentVersion, RunSnapshotID: l.RunSnapshotID, FactoryKey: bundle.FactoryKey, Bindings: bundle.MCPBindings}
			}
			artifact.Entries = append(artifact.Entries, RuntimeGraphEntry{
				AgentID:      bundle.Agent.AgentID,
				AgentVersion: bundle.Agent.AgentVersion,
				CLI:          cliEntry,
			})
			continue
		}
		opts, closer, buildErr := compiler.FrozenBuildOpts{}, io.Closer(nil), error(nil)
		if providerlessLoomBundle(bundle) && runtimeLLMDefaultErr != nil {
			cleanup()
			return nil, runtimeLLMDefaultErr
		}
		if providerlessLoomBundle(bundle) && runtimeLLMDefault != nil {
			if runtimeNilLike(l.CLIExecutor) {
				cleanup()
				return nil, runtimeHostUnsupportedError("CLI runtime executor is unavailable")
			}
			if runtimeNilLike(credentialResolver) {
				cleanup()
				return nil, credentials.ErrCredentialUnavailable
			}
			if err := credentialResolver.Validate(ctx, runtimeLLMDefault.AccessRef); err != nil {
				cleanup()
				if runtimeNilLike(err) {
					return nil, credentials.ErrCredentialUnavailable
				}
				return nil, err
			}
			runtimeRecord := runtimeLLMAgentRecord(bundle, *runtimeLLMDefault)
			runtimeLLM, runtimeErr := runtimellm.New(
				l.CLIExecutor,
				envelope.WorkspaceID,
				runtimeRecord,
				execution.AgentExecutionStamp{
					AgentID:        bundle.Agent.AgentID,
					AgentVersion:   int(bundle.Agent.AgentVersion),
					ExecutionScope: execution.ScopeTeamFreeCollab,
					RunSnapshotID:  l.RunSnapshotID,
				},
			)
			if runtimeErr != nil {
				cleanup()
				return nil, runtimeErr
			}
			if withLLM, ok := factory.(RuntimeHostFactoryWithLLM); ok {
				opts, closer, buildErr = withLLM.BuildWithLLM(ctx, bundle, credentialResolver, runtimeLLM)
			} else {
				opts, closer, buildErr = buildRuntimeHostsWithLLM(
					ctx,
					bundle,
					credentialResolver,
					newRuntimeMCPTransport,
					runtimeLLM,
				)
			}
		} else {
			opts, closer, buildErr = factory.Build(ctx, bundle, credentialResolver)
		}
		if buildErr != nil {
			cleanup()
			return nil, buildErr
		}
		if closer == nil {
			cleanup()
			return nil, compiler.ErrFactoryCompileFailed
		}
		artifact.closers = append(artifact.closers, closer)
		// Frozen graphs execute with the same logical usage tracking as
		// PreparedRun: a before-step binding hook plus the logical wrapper and
		// the physical usage boundary. Outside a usage-scoped run the wiring
		// is transparent, so direct graph execution keeps its old behavior.
		opts = loomruntime.InstallFrozenUsageTracking(opts)
		if bundle.FactoryKey == compiler.StandardFrozenToolsKey() {
			opts = loomruntime.InstallFrozenMemberJournal(opts)
		}

		var graph *loom.Graph
		if l.Registry == nil {
			graph, err = compiler.CompileFrozen(ctx, bundle, resolver, opts)
		} else {
			graph, err = compiler.CompileFrozenWithRegistry(ctx, l.Registry, bundle, resolver, opts)
		}
		if err != nil {
			cleanup()
			return nil, err
		}
		artifact.Entries = append(artifact.Entries, RuntimeGraphEntry{
			AgentID: bundle.Agent.AgentID, AgentVersion: bundle.Agent.AgentVersion, Graph: graph,
			Bundle: &bundle,
		})
	}
	return artifact, nil
}

// withPublishedServiceCredentialAuthorization treats the immutable execution
// snapshot as the user's exact grant to shared workspace services. References
// outside that snapshot remain denied, even when they name the same workspace.
func withPublishedServiceCredentialAuthorization(
	ctx context.Context,
	payload frozen.ArtifactPayloadV1,
) context.Context {
	allowed := make([]frozen.CredentialReference, 0)
	for _, bundle := range payload.Bundles {
		for _, ref := range bundle.Credentials {
			if ref.Scope == frozen.CredentialScopeWorkspaceService {
				allowed = append(allowed, ref)
			}
		}
	}
	for _, target := range payload.DeliveryTargets {
		if target.AccessRef.Scope == frozen.CredentialScopeWorkspaceService {
			allowed = append(allowed, target.AccessRef)
		}
		for _, binding := range target.CredentialBindings {
			if binding.CredentialRef.Scope == frozen.CredentialScopeWorkspaceService {
				allowed = append(allowed, binding.CredentialRef)
			}
		}
	}
	return frozen.WithServiceReferenceAuthorization(ctx, func(
		_ context.Context,
		subject execution.Subject,
		requested frozen.CredentialReference,
	) error {
		if subject.UserID == "" || subject.WorkspaceID != requested.WorkspaceID {
			return execution.ErrSubjectMismatch
		}
		for _, ref := range allowed {
			if sameCredentialReference(ref, requested) {
				return nil
			}
		}
		return execution.ErrSubjectMismatch
	})
}

func sameCredentialReference(left, right frozen.CredentialReference) bool {
	if left.SchemaVersion != right.SchemaVersion || left.Scope != right.Scope ||
		left.UserID != right.UserID || left.ServiceID != right.ServiceID ||
		left.WorkspaceID != right.WorkspaceID || left.Kind != right.Kind ||
		left.ResourceID != right.ResourceID || left.Slot != right.Slot {
		return false
	}
	if left.CredentialVersion == nil || right.CredentialVersion == nil {
		return left.CredentialVersion == nil && right.CredentialVersion == nil
	}
	return *left.CredentialVersion == *right.CredentialVersion
}

func providerlessLoomBundle(bundle frozen.FrozenExecutionBundle) bool {
	if bundle.Agent.Engine != "loom" {
		return false
	}
	if bundle.Agent.Model != "" || len(bundle.FallbackModels) != 0 {
		return false
	}
	return true
}

func runtimeDefaultForProviderlessLoom(bundles []frozen.FrozenExecutionBundle) (*frozen.FrozenRuntimeBinding, error) {
	var selected *frozen.FrozenRuntimeBinding
	for _, bundle := range bundles {
		if bundle.Runtime == nil || !engine.IsCLIEngine(bundle.Runtime.Engine) {
			continue
		}
		if selected != nil && selected.RuntimeID != bundle.Runtime.RuntimeID {
			return nil, runtimeHostUnsupportedError("runtime default is ambiguous for providerless Loom bundle")
		}
		if selected == nil || (selected.Engine != "codex" && bundle.Runtime.Engine == "codex") {
			runtime := *bundle.Runtime
			selected = &runtime
		}
	}
	return selected, nil
}

func runtimeCLIAgentRecord(bundle frozen.FrozenExecutionBundle) *registry.AgentRecord {
	agent := bundle.Agent
	profiles := make(map[string]stdlib.ProfileEntry, len(agent.Profiles))
	for name, profile := range agent.Profiles {
		profiles[name] = stdlib.ProfileEntry{
			SystemAddition: profile.SystemAddition,
			Greeting:       profile.Greeting,
		}
	}
	skills := make([]stdlib.SkillDef, 0, len(bundle.Skills))
	for _, skill := range bundle.Skills {
		skills = append(skills, stdlib.SkillDef{
			Name: skill.Name, Description: skill.Description,
			Body: skill.Body, AlwaysActive: skill.AlwaysActive,
		})
	}
	mcpServers := make([]registry.MCPServerConfig, 0, len(bundle.MCPBindings))
	for _, binding := range bundle.MCPBindings {
		mcpServers = append(mcpServers, registry.MCPServerConfig{
			ServerID:   binding.ServerID,
			URL:        binding.URL,
			Filter:     append([]string(nil), binding.Filter...),
			WriteTools: append([]string(nil), binding.WriteTools...),
		})
	}
	record := &registry.AgentRecord{
		Name: agent.Name, ID: agent.AgentID, WorkspaceID: agent.WorkspaceID,
		DisplayName: agent.DisplayName, Role: agent.Role,
		Engine: agent.Engine, RuntimeID: agent.RuntimeID,
		Version: int(agent.AgentVersion), Model: agent.Model,
		Spec: stdlib.AgentSpec{
			SystemPrompt: agent.SystemPrompt,
			Identity: stdlib.IdentitySpec{
				Core: agent.Identity.Core, Extended: agent.Identity.Extended, Raw: agent.Identity.Raw,
			},
			Profiles: profiles, Skills: skills, GraphType: agent.GraphType,
		},
		Permissions: registry.PermissionConfig{
			Deny:  append([]string(nil), agent.Permissions.Deny...),
			Allow: append([]string(nil), agent.Permissions.Allow...),
			Ask:   append([]string(nil), agent.Permissions.Ask...),
		},
		MCPServers: mcpServers,
		MaxCostUSD: agent.Limits.MaxCostUSD, MaxTokens: agent.Limits.MaxTokens,
		MaxOutputTokens: int(agent.Limits.MaxOutputTokens), StepBudget: agent.Limits.StepBudget,
		MaxToolRepeats:  int(agent.Limits.MaxToolRepeats),
		FallbackModels:  append([]string(nil), agent.Fallback.Models...),
		FallbackRetries: int(agent.Fallback.Retries), GraphType: agent.GraphType,
	}
	if agent.MemoryConfig != nil {
		record.MemoryConfig = &registry.MemoryConfig{
			Enabled: agent.MemoryConfig.Enabled, TopK: int(agent.MemoryConfig.TopK),
			AutoRemember: agent.MemoryConfig.AutoRemember, Scope: agent.MemoryConfig.Scope,
		}
	}
	record.MemorySlots = make([]registry.MemorySlot, 0, len(agent.MemorySlots))
	for _, slot := range agent.MemorySlots {
		record.MemorySlots = append(record.MemorySlots, registry.MemorySlot{
			Key: slot.Key, Label: slot.Label, Description: slot.Description,
		})
	}
	if len(agent.OutputSchema) > 0 && string(agent.OutputSchema) != "null" {
		outputSchema := append(json.RawMessage(nil), agent.OutputSchema...)
		record.OutputSchema = &outputSchema
	}
	if agent.Guard != nil {
		record.Guard = &registry.GuardConfig{
			Enabled: agent.Guard.Enabled, MaxInputLen: int(agent.Guard.MaxInputLen),
			BlockedTerms: append([]string(nil), agent.Guard.BlockedTerms...),
		}
	}
	if agent.Compaction != nil {
		record.Compaction = &registry.CompactionConfig{
			Enabled: agent.Compaction.Enabled, TokenThreshold: int(agent.Compaction.TokenThreshold),
		}
	}
	return record
}

func runtimeLLMAgentRecord(
	bundle frozen.FrozenExecutionBundle,
	runtimeBinding frozen.FrozenRuntimeBinding,
) *registry.AgentRecord {
	record := runtimeCLIAgentRecord(bundle)
	record.Engine = runtimeBinding.Engine
	record.Model = ""
	record.RuntimeID = runtimeBinding.RuntimeID
	record.RuntimePoolID = ""
	record.RuntimePolicyMode = "auto_single"
	record.FallbackModels = nil
	record.FallbackRetries = 0
	return record
}

func runtimeNilLike(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// DurableMember identifies the explicitly versioned member execution contract.
func (entry RuntimeGraphEntry) DurableMember() bool {
	return entry.Graph != nil && entry.Bundle != nil && entry.Bundle.FactoryKey == compiler.StandardFrozenToolsKey()
}
