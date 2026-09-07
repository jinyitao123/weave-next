package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/grounding"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/otel"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const (
	standardFrozenFactoryID      = "standard"
	standardFrozenFactoryVersion = "1"
	standardFrozenCompilerABI    = "weave-graph-abi-v1"
	standardFrozenSchemaID       = "weave-standard-factory-input/1"
)

type standardFrozenEnumerator struct{}

// NewStandardFrozenDescriptor returns the frozen ABI descriptor for the legacy
// standard graph topology. Registration remains an explicit caller decision.
func NewStandardFrozenDescriptor() GraphFactoryDescriptor {
	return GraphFactoryDescriptor{
		FactoryID:             standardFrozenFactoryID,
		FactoryVersion:        standardFrozenFactoryVersion,
		CompilerABI:           standardFrozenCompilerABI,
		EnumerateDependencies: standardFrozenEnumerator{},
		DescribeCapability: func(context.Context, frozen.FrozenAgentRecord) (frozen.CapabilityManifest, error) {
			return standardFrozenCapability(), nil
		},
		Compile: compileStandardFrozen,
	}
}

func (standardFrozenEnumerator) FreezeSchema() FreezeSchema {
	return FreezeSchema{SchemaID: standardFrozenSchemaID, SchemaVersion: frozen.FrozenSchemaVersion}
}

func (standardFrozenEnumerator) EncodeFactoryInput(
	_ context.Context,
	record registry.AgentRecord,
	_ CredentialRefEncoder,
) (json.RawMessage, error) {
	if err := ValidateFrozenSkillRefs(record); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

func (standardFrozenEnumerator) EnumerateDependencies(
	ctx context.Context,
	agent frozen.FrozenAgentRecord,
	metadata MetadataResolver,
) (frozen.EnumeratedDependencyManifest, error) {
	if err := requireStandardFrozenFactoryInput(agent.FactoryInput, CodeDependencyUnenumerable); err != nil {
		return frozen.EnumeratedDependencyManifest{}, err
	}
	models := append([]string{agent.Model}, agent.Fallback.Models...)
	// CLI model names are interpreted by the bound runtime, whose credentials
	// are local. They are not workspace API-provider dependencies.
	if agent.Engine != "loom" {
		models = nil
	}
	dependencyCapacity := len(models)
	if agent.Engine != "loom" {
		dependencyCapacity++
	}
	dependencies := make([]frozen.EnumeratedDependencyRef, 0, dependencyCapacity)
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == "" {
			continue
		}
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		ownerVersion := agent.AgentVersion
		resolved, err := metadata.ResolveMetadata(ctx, frozen.EnumeratedDependencyRef{
			WorkspaceID: agent.WorkspaceID, OwnerType: "agent", OwnerID: agent.AgentID,
			OwnerAgentVersion: &ownerVersion, DependencyType: "model_binding",
			DependencyKey: model,
		})
		if err != nil {
			return frozen.EnumeratedDependencyManifest{}, err
		}
		dependencies = append(dependencies, resolved.Ref)
	}
	if agent.Engine != "loom" {
		if agent.RuntimeID == "" {
			return frozen.EnumeratedDependencyManifest{}, normalizeCompilerError(
				CodeDependencyUnenumerable,
				fmt.Errorf("CLI agent runtime_id is required"),
			)
		}
		ownerVersion := agent.AgentVersion
		resolved, err := metadata.ResolveMetadata(ctx, frozen.EnumeratedDependencyRef{
			WorkspaceID: agent.WorkspaceID, OwnerType: "agent", OwnerID: agent.AgentID,
			OwnerAgentVersion: &ownerVersion, DependencyType: "runtime_binding",
			DependencyKey: agent.RuntimeID,
		})
		if err != nil {
			return frozen.EnumeratedDependencyManifest{}, err
		}
		dependencies = append(dependencies, resolved.Ref)
	}
	skillDependencies, err := EnumerateFrozenSkillRefs(ctx, agent, metadata)
	if err != nil {
		return frozen.EnumeratedDependencyManifest{}, err
	}
	dependencies = append(dependencies, skillDependencies...)
	return frozen.EnumeratedDependencyManifest{
		SchemaVersion: frozen.FrozenSchemaVersion,
		Dependencies:  dependencies,
	}, nil
}

func compileStandardFrozen(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	_ FrozenResolver,
	opts FrozenBuildOpts,
) (*loom.Graph, frozen.CapabilityManifest, error) {
	return compileStandardFrozenVersion(ctx, bundle, opts, standardFrozenFactoryVersion)
}

func compileStandardFrozenVersion(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	opts FrozenBuildOpts,
	version string,
) (*loom.Graph, frozen.CapabilityManifest, error) {
	capability := standardFrozenCapability()
	if err := validateStandardFrozenBundleVersion(bundle, version); err != nil {
		return nil, capability, err
	}

	record, compileOpts, err := mapStandardFrozenToLegacy(bundle, opts)
	if err != nil {
		return nil, capability, err
	}
	compileOpts.DurableMember = version == StandardFrozenToolsVersion
	graph, err := CompileAgent(
		bundle.Agent.WorkspaceID,
		&record,
		opts.LLM,
		opts.Tools,
		compileOpts,
	)
	if err != nil {
		return nil, capability, normalizeCompilerError(CodeFactoryCompileFailed, err)
	}
	installStandardFrozenPromptStep(graph, bundle, record, compileOpts)
	return graph, capability, nil
}

func validateStandardFrozenBundle(bundle frozen.FrozenExecutionBundle) error {
	return validateStandardFrozenBundleVersion(bundle, standardFrozenFactoryVersion)
}

func validateStandardFrozenBundleVersion(bundle frozen.FrozenExecutionBundle, version string) error {
	wantKey := (frozen.FactoryKey{
		FactoryID:      standardFrozenFactoryID,
		FactoryVersion: version,
		CompilerABI:    standardFrozenCompilerABI,
	})
	if bundle.FactoryKey != wantKey {
		return standardFrozenCompileError("factory key does not match standard frozen descriptor")
	}
	if bundle.Agent.GraphType != standardFrozenFactoryID {
		return standardFrozenCompileError("agent graph_type is not standard")
	}
	if version == standardFrozenFactoryVersion {
		if err := requireStandardFrozenFactoryInput(bundle.Agent.FactoryInput, CodeFactoryCompileFailed); err != nil {
			return err
		}
	} else if err := ValidateStandardMCPBindings(bundle); err != nil {
		return err
	}
	if bundle.Agent.Engine == "loom" {
		if bundle.Agent.RuntimeID != "" || bundle.Runtime != nil {
			return standardFrozenCompileError("loom agent must not have a runtime binding")
		}
	} else if bundle.Agent.RuntimeID == "" ||
		bundle.Runtime == nil ||
		bundle.Runtime.RuntimeID != bundle.Agent.RuntimeID ||
		bundle.Runtime.Engine != bundle.Agent.Engine {
		return standardFrozenCompileError("CLI agent requires a matching runtime binding")
	}
	if bundle.Agent.Engine == "loom" {
		hasAgentModel := bundle.Agent.Model != ""
		hasPrimaryModel := bundle.PrimaryModel.ModelID != ""
		if hasAgentModel != hasPrimaryModel ||
			(hasAgentModel && bundle.Agent.Model != bundle.PrimaryModel.ModelID) {
			return standardFrozenCompileError("primary model binding does not match agent model")
		}
		if !hasAgentModel && len(bundle.Agent.Fallback.Models) > 0 {
			return standardFrozenCompileError("fallback models require a primary model binding")
		}
		if len(bundle.Agent.Fallback.Models) != len(bundle.FallbackModels) {
			return standardFrozenCompileError("fallback model bindings do not match agent fallback models")
		}
		seen := make(map[string]struct{}, len(bundle.FallbackModels)+1)
		if hasPrimaryModel {
			seen[bundle.PrimaryModel.ModelID] = struct{}{}
		}
		for index := range bundle.FallbackModels {
			bindingModel := bundle.FallbackModels[index].ModelID
			if bindingModel == "" || bundle.Agent.Fallback.Models[index] != bindingModel {
				return standardFrozenCompileError("fallback model bindings do not match agent fallback models")
			}
			if _, duplicate := seen[bindingModel]; duplicate {
				return standardFrozenCompileError("model bindings contain duplicate model ids")
			}
			seen[bindingModel] = struct{}{}
		}
	} else if bundle.PrimaryModel.ModelID != "" || len(bundle.FallbackModels) != 0 {
		return standardFrozenCompileError("CLI models must use the runtime binding")
	}

	for _, skill := range bundle.Skills {
		if skill.SourceType == "builtin" && skill.Body == "" {
			return standardFrozenCompileError("frozen builtin skill body is empty")
		}
	}
	return nil
}

func mapStandardFrozenToLegacy(
	bundle frozen.FrozenExecutionBundle,
	opts FrozenBuildOpts,
) (registry.AgentRecord, CompileOpts, error) {
	agentVersion, err := standardFrozenInt(bundle.Agent.AgentVersion, "agent_version")
	if err != nil {
		return registry.AgentRecord{}, CompileOpts{}, err
	}
	maxOutputTokens, err := standardFrozenInt(bundle.Agent.Limits.MaxOutputTokens, "max_output_tokens")
	if err != nil {
		return registry.AgentRecord{}, CompileOpts{}, err
	}
	maxToolRepeats, err := standardFrozenInt(bundle.Agent.Limits.MaxToolRepeats, "max_tool_repeats")
	if err != nil {
		return registry.AgentRecord{}, CompileOpts{}, err
	}
	fallbackRetries, err := standardFrozenInt(bundle.Agent.Fallback.Retries, "fallback_retries")
	if err != nil {
		return registry.AgentRecord{}, CompileOpts{}, err
	}

	profiles := make(map[string]stdlib.ProfileEntry, len(bundle.Agent.Profiles))
	for name, profile := range bundle.Agent.Profiles {
		profiles[name] = stdlib.ProfileEntry{
			SystemAddition: profile.SystemAddition,
			Greeting:       profile.Greeting,
		}
	}
	fallbackModels := make([]string, len(bundle.FallbackModels))
	for index := range bundle.FallbackModels {
		fallbackModels[index] = bundle.FallbackModels[index].ModelID
	}
	if bundle.Agent.Engine != "loom" {
		fallbackModels = append([]string(nil), bundle.Agent.Fallback.Models...)
	}
	memorySlots := make([]registry.MemorySlot, len(bundle.Agent.MemorySlots))
	for index, slot := range bundle.Agent.MemorySlots {
		memorySlots[index] = registry.MemorySlot{
			Key: slot.Key, Label: slot.Label, Description: slot.Description,
		}
	}

	record := registry.AgentRecord{
		Name:            bundle.Agent.Name,
		ID:              bundle.Agent.AgentID,
		WorkspaceID:     bundle.Agent.WorkspaceID,
		DisplayName:     bundle.Agent.DisplayName,
		Role:            bundle.Agent.Role,
		Engine:          bundle.Agent.Engine,
		RuntimeID:       bundle.Agent.RuntimeID,
		Version:         agentVersion,
		Model:           bundle.Agent.Model,
		Spec:            mapStandardFrozenSpec(bundle.Agent, profiles),
		Permissions:     mapStandardFrozenPermissions(bundle.Agent.Permissions),
		MemorySlots:     memorySlots,
		OutputSchema:    standardFrozenOutputSchema(bundle.Agent.OutputSchema),
		MaxCostUSD:      bundle.Agent.Limits.MaxCostUSD,
		MaxTokens:       bundle.Agent.Limits.MaxTokens,
		MaxOutputTokens: maxOutputTokens,
		StepBudget:      bundle.Agent.Limits.StepBudget,
		MaxToolRepeats:  maxToolRepeats,
		FallbackModels:  fallbackModels,
		FallbackRetries: fallbackRetries,
		GraphType:       standardFrozenFactoryID,
	}
	if bundle.Agent.MemoryConfig != nil {
		memoryConfig, memoryErr := mapStandardFrozenMemory(*bundle.Agent.MemoryConfig)
		if memoryErr != nil {
			return registry.AgentRecord{}, CompileOpts{}, memoryErr
		}
		record.MemoryConfig = &memoryConfig
	}
	if bundle.Agent.Guard != nil {
		guard, guardErr := mapStandardFrozenGuard(*bundle.Agent.Guard)
		if guardErr != nil {
			return registry.AgentRecord{}, CompileOpts{}, guardErr
		}
		record.Guard = &guard
	}
	if bundle.Agent.Compaction != nil {
		compaction, compactionErr := mapStandardFrozenCompaction(*bundle.Agent.Compaction)
		if compactionErr != nil {
			return registry.AgentRecord{}, CompileOpts{}, compactionErr
		}
		record.Compaction = &compaction
	}
	// Context management defaults to enabled when the frozen record omits it.
	if record.Compaction == nil {
		record.Compaction = registry.DefaultCompactionConfig()
	}
	if record.MemoryConfig == nil {
		record.MemoryConfig = registry.DefaultMemoryConfig()
	}

	beforeHooks := append([]loom.StepHook(nil), opts.Hooks.BeforeStepHooks...)
	afterHooks := make([]loom.StepHook, 0, len(opts.Hooks.AfterStepHooks)+2)
	if !isNilCompilerValue(opts.AuditStore) {
		beforeHooks = append(beforeHooks, otel.TraceStart(bundle.Agent.WorkspaceID))
		afterHooks = append(
			afterHooks,
			otel.TraceEnd(opts.AuditStore),
			otel.AuditHook(opts.AuditStore, bundle.Agent.WorkspaceID, bundle.Agent.Name),
		)
	}
	if record.MemoryConfig != nil &&
		record.MemoryConfig.Enabled &&
		record.MemoryConfig.AutoRemember &&
		opts.MemoryService != nil {
		afterHooks = append(afterHooks, memory.AutoRememberHook(
			opts.LLM,
			opts.MemoryService,
			bundle.PrimaryModel.ModelID,
			bundle.Agent.WorkspaceID,
			bundle.Agent.Name,
			record.MemoryConfig.Scope,
		))
	}
	if record.MaxCostUSD > 0 {
		afterHooks = append(afterHooks, stdlib.CostBudgetHook(record.MaxCostUSD))
		record.MaxCostUSD = 0
	}
	if record.MaxTokens > 0 {
		afterHooks = append(afterHooks, stdlib.TokenBudgetHook(record.MaxTokens))
		record.MaxTokens = 0
	}
	afterHooks = append(afterHooks, opts.Hooks.AfterStepHooks...)
	compileOpts := CompileOpts{
		ToolHooks:           append([]contract.ToolHook(nil), opts.Hooks.ToolHooks...),
		BeforeStepHooks:     beforeHooks,
		AfterStepHooks:      afterHooks,
		ExecutionLLMWrapper: opts.ExecutionLLMWrapper,
	}
	if record.MemoryConfig != nil &&
		record.MemoryConfig.Enabled &&
		opts.MemoryService != nil {
		memoryTopK := record.MemoryConfig.TopK
		if memoryTopK <= 0 {
			memoryTopK = 5
		}
		compileOpts.MemoryService = opts.MemoryService
		compileOpts.MemoryTopK = memoryTopK
		compileOpts.MemoryScope = record.MemoryConfig.Scope
	}
	return record, compileOpts, nil
}

func installStandardFrozenPromptStep(
	graph *loom.Graph,
	bundle frozen.FrozenExecutionBundle,
	record registry.AgentRecord,
	opts CompileOpts,
) {
	identity := record.Spec.Identity
	if identity.Core == "" && bundle.Agent.SystemPrompt != "" {
		identity.Core = bundle.Agent.SystemPrompt
	}
	identity.Core = grounding.GroundIdentity(identity.Core)

	matchableSkills := make([]stdlib.SkillDef, 0, len(bundle.Skills))
	for _, skill := range bundle.Skills {
		definition := stdlib.SkillDef{
			Name: skill.Name, Description: skill.Description, Body: skill.Body,
			AlwaysActive: skill.AlwaysActive, Scripts: []string{}, References: []string{},
		}
		if skill.SourceType == "builtin" || (skill.AlwaysActive && skill.Body != "") {
			identity.Core += "\n\n## 技能: " + skill.Name + "\n以下规则必须遵守：\n" + skill.Body
			continue
		}
		matchableSkills = append(matchableSkills, definition)
	}

	var matcher stdlib.SkillMatcher = &stdlib.KeywordMatcher{}
	if opts.Embedder != nil {
		matcher = NewSemanticMatcher(opts.Embedder)
	}
	promptTarget := "chat"
	if opts.MemoryService != nil {
		promptTarget = "memory_retrieve"
	}
	graph.AddStep("prompt_assemble", stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
		Identity:              identity,
		Skills:                matchableSkills,
		Profile:               opts.Profile,
		ProfilesMap:           record.Spec.Profiles,
		Context:               opts.Context,
		MaxSystemPromptTokens: 8000,
		SkillMatcher:          matcher,
	}), loom.Always(promptTarget))

	topology := graph.Topology()
	for index := range topology {
		if topology[index].Name == "prompt_assemble" {
			topology[index].Detail = fmt.Sprintf("%d skills", len(bundle.Skills))
		}
	}
	graph.SetTopology(topology)
}

func mapStandardFrozenSpec(
	agent frozen.FrozenAgentRecord,
	profiles map[string]stdlib.ProfileEntry,
) stdlib.AgentSpec {
	return stdlib.AgentSpec{
		SystemPrompt: agent.SystemPrompt,
		Identity: stdlib.IdentitySpec{
			Core: agent.Identity.Core, Extended: agent.Identity.Extended, Raw: agent.Identity.Raw,
		},
		Profiles: profiles,
		Skills:   []stdlib.SkillDef{},
	}
}

func mapStandardFrozenPermissions(value frozen.FrozenPermissions) registry.PermissionConfig {
	return registry.PermissionConfig{
		Deny:  append([]string(nil), value.Deny...),
		Allow: append([]string(nil), value.Allow...),
		Ask:   append([]string(nil), value.Ask...),
	}
}

func mapStandardFrozenMemory(value frozen.FrozenMemoryConfig) (registry.MemoryConfig, error) {
	topK, err := standardFrozenInt(value.TopK, "memory_top_k")
	if err != nil {
		return registry.MemoryConfig{}, err
	}
	return registry.MemoryConfig{
		Enabled: value.Enabled, TopK: topK, AutoRemember: value.AutoRemember, Scope: value.Scope,
	}, nil
}

func mapStandardFrozenGuard(value frozen.FrozenGuard) (registry.GuardConfig, error) {
	maxInputLen, err := standardFrozenInt(value.MaxInputLen, "guard.max_input_len")
	if err != nil {
		return registry.GuardConfig{}, err
	}
	return registry.GuardConfig{
		Enabled: value.Enabled, MaxInputLen: maxInputLen,
		BlockedTerms: append([]string(nil), value.BlockedTerms...),
	}, nil
}

func mapStandardFrozenCompaction(value frozen.FrozenCompaction) (registry.CompactionConfig, error) {
	tokenThreshold, err := standardFrozenInt(value.TokenThreshold, "compaction.token_threshold")
	if err != nil {
		return registry.CompactionConfig{}, err
	}
	return registry.CompactionConfig{Enabled: value.Enabled, TokenThreshold: tokenThreshold}, nil
}

func standardFrozenOutputSchema(raw json.RawMessage) *json.RawMessage {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	cloned := append(json.RawMessage(nil), raw...)
	return &cloned
}

func standardFrozenInt(value int64, field string) (int, error) {
	converted := int(value)
	if int64(converted) != value {
		return 0, standardFrozenCompileError(field + " cannot be represented as int")
	}
	return converted, nil
}

func requireStandardFrozenFactoryInput(raw json.RawMessage, fallbackCode string) error {
	if !bytes.Equal(raw, []byte(`{}`)) {
		return normalizeCompilerError(
			fallbackCode,
			fmt.Errorf("standard factory_input must be exactly {}"),
		)
	}
	return nil
}

func standardFrozenCompileError(message string) error {
	return normalizeCompilerError(CodeFactoryCompileFailed, fmt.Errorf("%s", message))
}

func standardFrozenCapability() frozen.CapabilityManifest {
	return frozen.CapabilityManifest{
		SchemaVersion:      frozen.FrozenSchemaVersion,
		MayYield:           false,
		InteractiveStepIDs: []string{},
		InteractiveToolIDs: []string{},
		MayInvokeAgent:     false,
		AgentStepIDs:       []string{},
	}
}
