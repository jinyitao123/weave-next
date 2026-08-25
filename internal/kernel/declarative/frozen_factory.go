package declarative

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/declarative/schema"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/grounding"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const (
	frozenFactoryVersion = "1"
	frozenCompilerABI    = "weave-graph-abi-v1"
	frozenSchemaID       = "weave-declarative-factory-input"
)

type frozenDescriptorEnumerator struct{}

type frozenFactoryInputWire struct {
	Entry string           `json:"entry"`
	Steps []frozenStepWire `json:"steps"`
}

type frozenFactoryInputDecodeWire struct {
	Entry *string           `json:"entry"`
	Steps []json.RawMessage `json:"steps"`
}

type frozenStepWire struct {
	Name      string               `json:"name"`
	Type      string               `json:"type"`
	Display   string               `json:"display"`
	Next      *string              `json:"next,omitempty"`
	Condition *frozenConditionWire `json:"condition,omitempty"`
	Config    json.RawMessage      `json:"config"`
}

type frozenStepDecodeWire struct {
	Name      *string              `json:"name"`
	Type      *string              `json:"type"`
	Display   *string              `json:"display"`
	Next      *string              `json:"next,omitempty"`
	Condition *frozenConditionWire `json:"condition,omitempty"`
	Config    json.RawMessage      `json:"config"`
}

type frozenConditionWire struct {
	Key       *string `json:"key"`
	TrueStep  *string `json:"true"`
	FalseStep *string `json:"false"`
}

type frozenChatConfigWire struct {
	Model         *string `json:"model,omitempty"`
	SystemPrompt  *string `json:"system_prompt,omitempty"`
	MaxIterations *int    `json:"max_iterations,omitempty"`
	MaxLoops      *int    `json:"max_loops,omitempty"`
}

type frozenLLMCallConfigWire struct {
	Model          *string                   `json:"model,omitempty"`
	PromptTemplate *string                   `json:"prompt_template,omitempty"`
	InputKeys      *[]string                 `json:"input_keys,omitempty"`
	OutputKey      *string                   `json:"output_key,omitempty"`
	Stream         *bool                     `json:"stream,omitempty"`
	MaxLoops       *int                      `json:"max_loops,omitempty"`
	Extract        *frozenLLMCallExtractWire `json:"extract,omitempty"`
}

type frozenLLMCallExtractWire struct {
	Key          *string   `json:"key,omitempty"`
	Mode         *string   `json:"mode,omitempty"`
	KeywordsTrue *[]string `json:"keywords_true,omitempty"`
}

type frozenLLMCheckConfigWire struct {
	Model          *string   `json:"model,omitempty"`
	PromptTemplate *string   `json:"prompt_template,omitempty"`
	InputKeys      *[]string `json:"input_keys,omitempty"`
	OutputKey      *string   `json:"output_key,omitempty"`
	ExtractMode    *string   `json:"extract_mode,omitempty"`
	JSONField      *string   `json:"json_field,omitempty"`
	KeywordsTrue   *[]string `json:"keywords_true,omitempty"`
	MaxLoops       *int      `json:"max_loops,omitempty"`
}

type frozenYieldConfigWire struct {
	YieldType *string `json:"yield_type,omitempty"`
	MaxLoops  *int    `json:"max_loops,omitempty"`
}

type frozenTransformConfigWire struct {
	Operations []json.RawMessage `json:"operations,omitempty"`
	MaxLoops   *int              `json:"max_loops,omitempty"`
}

type frozenBuiltinConfigWire struct {
	BuiltinType *string `json:"builtin_type,omitempty"`
	MaxLoops    *int    `json:"max_loops,omitempty"`
}

type frozenOperationTagWire struct {
	Op string `json:"op"`
}

type frozenSetOperationWire struct {
	Op     string          `json:"op"`
	Target string          `json:"target"`
	Value  json.RawMessage `json:"value"`
}

type frozenConcatOperationWire struct {
	Op        string   `json:"op"`
	Target    string   `json:"target"`
	Keys      []string `json:"keys"`
	Separator string   `json:"separator,omitempty"`
}

type frozenCopyOperationWire struct {
	Op     string `json:"op"`
	Target string `json:"target"`
	Source string `json:"source"`
}

// NewFrozenDescriptor returns the immutable declarative v1 compiler contract.
func NewFrozenDescriptor() compiler.GraphFactoryDescriptor {
	return compiler.GraphFactoryDescriptor{
		FactoryID:             "declarative",
		FactoryVersion:        frozenFactoryVersion,
		CompilerABI:           frozenCompilerABI,
		EnumerateDependencies: frozenDescriptorEnumerator{},
		DescribeCapability:    describeFrozenDeclarativeCapability,
		Compile:               compileFrozenDeclarative,
	}
}

func describeFrozenDeclarativeCapability(
	_ context.Context,
	agent frozen.FrozenAgentRecord,
) (frozen.CapabilityManifest, error) {
	definition, err := decodeFrozenFactoryInput(agent.FactoryInput)
	if err != nil {
		return frozen.CapabilityManifest{}, frozenFactoryCompileError(err)
	}
	interactive := make([]string, 0)
	agentSteps := make([]string, 0)
	for _, step := range definition.Steps {
		if step.Type == "yield" {
			interactive = append(interactive, step.Name)
		}
		if step.Type == "worker" {
			agentSteps = append(agentSteps, step.Name)
		}
	}
	sort.Strings(interactive)
	sort.Strings(agentSteps)
	return frozen.CapabilityManifest{
		SchemaVersion:      frozen.CapabilityManifestSchemaVersion,
		MayYield:           len(interactive) > 0,
		InteractiveStepIDs: interactive,
		InteractiveToolIDs: []string{},
		MayInvokeAgent:     len(agentSteps) > 0,
		AgentStepIDs:       agentSteps,
	}, nil
}

func (frozenDescriptorEnumerator) FreezeSchema() compiler.FreezeSchema {
	return compiler.FreezeSchema{SchemaID: frozenSchemaID, SchemaVersion: frozen.FrozenSchemaVersion}
}

func (frozenDescriptorEnumerator) EncodeFactoryInput(
	_ context.Context,
	record registry.AgentRecord,
	_ compiler.CredentialRefEncoder,
) (json.RawMessage, error) {
	if err := compiler.ValidateFrozenSkillRefs(record); err != nil {
		return nil, err
	}
	if record.GraphDefinition == nil {
		return nil, fmt.Errorf("declarative graph_definition is required")
	}
	wire := frozenFactoryInputWire{
		Entry: record.GraphDefinition.Entry,
		Steps: make([]frozenStepWire, len(record.GraphDefinition.Steps)),
	}
	for index, step := range record.GraphDefinition.Steps {
		config, err := encodeFrozenConfig(step.Type, step.Config)
		if err != nil {
			return nil, fmt.Errorf("step %q config: %w", step.Name, err)
		}
		wireStep := frozenStepWire{
			Name: step.Name, Type: step.Type, Display: step.Display,
			Next: step.Next, Config: config,
		}
		if step.Condition != nil {
			wireStep.Condition = &frozenConditionWire{
				Key:       stringPointer(step.Condition.Key),
				TrueStep:  cloneStringPointer(step.Condition.TrueStep),
				FalseStep: cloneStringPointer(step.Condition.FalseStep),
			}
		}
		wire.Steps[index] = wireStep
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode declarative factory_input: %w", err)
	}
	if _, err := decodeFrozenFactoryInput(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (frozenDescriptorEnumerator) EnumerateDependencies(
	ctx context.Context,
	agent frozen.FrozenAgentRecord,
	metadata compiler.MetadataResolver,
) (frozen.EnumeratedDependencyManifest, error) {
	models := append([]string{agent.Model}, agent.Fallback.Models...)
	dependencies := make([]frozen.EnumeratedDependencyRef, 0, len(models))
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
	skillDependencies, err := compiler.EnumerateFrozenSkillRefs(ctx, agent, metadata)
	if err != nil {
		return frozen.EnumeratedDependencyManifest{}, err
	}
	dependencies = append(dependencies, skillDependencies...)
	return frozen.EnumeratedDependencyManifest{
		SchemaVersion: frozen.FrozenSchemaVersion,
		Dependencies:  dependencies,
	}, nil
}

func compileFrozenDeclarative(
	_ context.Context,
	bundle frozen.FrozenExecutionBundle,
	_ compiler.FrozenResolver,
	opts compiler.FrozenBuildOpts,
) (*loom.Graph, frozen.CapabilityManifest, error) {
	definition, err := decodeFrozenFactoryInput(bundle.Agent.FactoryInput)
	if err != nil {
		return nil, frozen.CapabilityManifest{}, frozenFactoryCompileError(err)
	}
	if err := bindFrozenModels(&definition, bundle); err != nil {
		return nil, frozen.CapabilityManifest{}, frozenFactoryCompileError(err)
	}
	if err := validateFrozenRuntimeDependencies(definition, opts); err != nil {
		return nil, frozen.CapabilityManifest{}, frozenFactoryCompileError(err)
	}
	record, err := frozenRegistryRecord(bundle)
	if err != nil {
		return nil, frozen.CapabilityManifest{}, frozenFactoryCompileError(err)
	}

	graph := loom.NewGraph(
		bundle.Agent.WorkspaceID+":"+bundle.Agent.Name,
		definition.Entry,
		loom.WithStepBudget(100),
		loom.WithCheckpointHistory(50),
	)
	topology := make([]loom.StepInfo, 0, len(definition.Steps))
	interactive := make([]string, 0)
	for _, stepDefinition := range definition.Steps {
		var step loom.Step
		switch {
		case stepDefinition.Type == "chat":
			step = buildFrozenChatStep(stepDefinition.Config, opts)
		case stepDefinition.Type == "builtin" &&
			getStr(stepDefinition.Config, "builtin_type", "") == "prompt_assemble":
			step = buildFrozenPromptAssembleStep(&record)
		default:
			step = buildStep(
				stepDefinition,
				opts.LLM,
				opts.Tools,
				&record,
				compiler.CompileOpts{MemoryService: opts.MemoryService},
			)
		}
		graph.AddStep(
			stepDefinition.Name,
			wrapWithSSE(step, stepDefinition.Display),
			buildRouter(stepDefinition),
		)
		topology = append(topology, buildStepInfo(stepDefinition))
		if stepDefinition.Type == "yield" {
			interactive = append(interactive, stepDefinition.Name)
		}
	}
	graph.SetTopology(topology)
	before := append([]loom.StepHook(nil), opts.Hooks.BeforeStepHooks...)
	after := append([]loom.StepHook(nil), opts.Hooks.AfterStepHooks...)
	if len(before) > 0 || len(after) > 0 {
		graph.SetHooks(loom.HookPoints{Before: before, After: after})
	}
	sort.Strings(interactive)
	return graph, frozen.CapabilityManifest{
		SchemaVersion:      frozen.FrozenSchemaVersion,
		MayYield:           len(interactive) > 0,
		InteractiveStepIDs: interactive,
		InteractiveToolIDs: []string{},
		MayInvokeAgent:     false,
		AgentStepIDs:       []string{},
	}, nil
}

func encodeFrozenConfig(stepType string, config map[string]any) (json.RawMessage, error) {
	switch stepType {
	case "chat":
		if err := requireStepSchemaKeys(config, "chat"); err != nil {
			return nil, err
		}
		model, err := frozenOptionalString(config, "model")
		if err != nil {
			return nil, err
		}
		systemPrompt, err := frozenOptionalString(config, "system_prompt")
		if err != nil {
			return nil, err
		}
		maxIterations, err := frozenOptionalInt(config, "max_iterations")
		if err != nil {
			return nil, err
		}
		maxLoops, err := frozenOptionalInt(config, "max_loops")
		if err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenChatConfigWire{
			Model: model, SystemPrompt: systemPrompt,
			MaxIterations: maxIterations, MaxLoops: maxLoops,
		})
	case "llm_call":
		if err := requireStepSchemaKeys(config, "llm_call"); err != nil {
			return nil, err
		}
		model, err := frozenOptionalString(config, "model")
		if err != nil {
			return nil, err
		}
		prompt, err := frozenOptionalString(config, "prompt_template")
		if err != nil {
			return nil, err
		}
		inputKeys, err := frozenOptionalStrings(config, "input_keys")
		if err != nil {
			return nil, err
		}
		outputKey, err := frozenOptionalString(config, "output_key")
		if err != nil {
			return nil, err
		}
		stream, err := frozenOptionalBool(config, "stream")
		if err != nil {
			return nil, err
		}
		maxLoops, err := frozenOptionalInt(config, "max_loops")
		if err != nil {
			return nil, err
		}
		extract, err := encodeFrozenExtract(config)
		if err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenLLMCallConfigWire{
			Model: model, PromptTemplate: prompt, InputKeys: inputKeys,
			OutputKey: outputKey, Stream: stream, MaxLoops: maxLoops, Extract: extract,
		})
	case "llm_check":
		if err := requireStepSchemaKeys(config, "llm_check"); err != nil {
			return nil, err
		}
		model, err := frozenOptionalString(config, "model")
		if err != nil {
			return nil, err
		}
		prompt, err := frozenOptionalString(config, "prompt_template")
		if err != nil {
			return nil, err
		}
		inputKeys, err := frozenOptionalStrings(config, "input_keys")
		if err != nil {
			return nil, err
		}
		outputKey, err := frozenOptionalString(config, "output_key")
		if err != nil {
			return nil, err
		}
		extractMode, err := frozenOptionalString(config, "extract_mode")
		if err != nil {
			return nil, err
		}
		jsonField, err := frozenOptionalString(config, "json_field")
		if err != nil {
			return nil, err
		}
		keywords, err := frozenOptionalStrings(config, "keywords_true")
		if err != nil {
			return nil, err
		}
		maxLoops, err := frozenOptionalInt(config, "max_loops")
		if err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenLLMCheckConfigWire{
			Model: model, PromptTemplate: prompt, InputKeys: inputKeys,
			OutputKey: outputKey, ExtractMode: extractMode, JSONField: jsonField,
			KeywordsTrue: keywords, MaxLoops: maxLoops,
		})
	case "yield":
		if err := requireStepSchemaKeys(config, "yield"); err != nil {
			return nil, err
		}
		yieldType, err := frozenOptionalString(config, "yield_type")
		if err != nil {
			return nil, err
		}
		maxLoops, err := frozenOptionalInt(config, "max_loops")
		if err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenYieldConfigWire{YieldType: yieldType, MaxLoops: maxLoops})
	case "transform":
		if err := requireStepSchemaKeys(config, "transform"); err != nil {
			return nil, err
		}
		operations, err := encodeFrozenOperations(config)
		if err != nil {
			return nil, err
		}
		maxLoops, err := frozenOptionalInt(config, "max_loops")
		if err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenTransformConfigWire{Operations: operations, MaxLoops: maxLoops})
	case "builtin":
		if err := requireStepSchemaKeys(config, "builtin"); err != nil {
			return nil, err
		}
		builtinType, err := frozenOptionalString(config, "builtin_type")
		if err != nil {
			return nil, err
		}
		maxLoops, err := frozenOptionalInt(config, "max_loops")
		if err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenBuiltinConfigWire{BuiltinType: builtinType, MaxLoops: maxLoops})
	case "worker":
		return nil, fmt.Errorf("worker steps are not supported by declarative frozen v1")
	default:
		return nil, fmt.Errorf("unknown declarative step type %q", stepType)
	}
}

func encodeFrozenExtract(config map[string]any) (*frozenLLMCallExtractWire, error) {
	value, exists := config["extract"]
	if !exists {
		return nil, nil
	}
	extract, ok := value.(map[string]any)
	if !ok || extract == nil {
		return nil, fmt.Errorf("extract must be an object")
	}
	if err := requireAllowedFrozenKeys(extract, schema.Extract().AllowedKeys()...); err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	key, err := frozenOptionalString(extract, "key")
	if err != nil {
		return nil, err
	}
	mode, err := frozenOptionalString(extract, "mode")
	if err != nil {
		return nil, err
	}
	keywords, err := frozenOptionalStrings(extract, "keywords_true")
	if err != nil {
		return nil, err
	}
	return &frozenLLMCallExtractWire{Key: key, Mode: mode, KeywordsTrue: keywords}, nil
}

func encodeFrozenOperations(config map[string]any) ([]json.RawMessage, error) {
	value, exists := config["operations"]
	if !exists {
		return nil, nil
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Slice {
		return nil, fmt.Errorf("operations must be an array")
	}
	operations := make([]json.RawMessage, reflected.Len())
	for index := 0; index < reflected.Len(); index++ {
		operation, ok := reflected.Index(index).Interface().(map[string]any)
		if !ok || operation == nil {
			return nil, fmt.Errorf("operation %d must be an object", index)
		}
		encoded, err := encodeFrozenOperation(operation)
		if err != nil {
			return nil, fmt.Errorf("operation %d: %w", index, err)
		}
		operations[index] = encoded
	}
	return operations, nil
}

func encodeFrozenOperation(operation map[string]any) (json.RawMessage, error) {
	op, err := frozenRequiredString(operation, "op")
	if err != nil {
		return nil, err
	}
	opSchema, ok := schema.OperationFor(op)
	if !ok {
		return nil, fmt.Errorf("unknown transform operation %q", op)
	}
	if err := requireAllowedFrozenKeys(operation, opSchema.AllowedKeys()...); err != nil {
		return nil, err
	}
	target, err := frozenRequiredString(operation, "target")
	if err != nil {
		return nil, err
	}
	switch op {
	case "set":
		value, exists := operation["value"]
		if !exists || value == nil {
			return nil, fmt.Errorf("value is required and must not be null")
		}
		rawValue, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("value is not JSON: %w", err)
		}
		if err := rejectFrozenNulls(rawValue); err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenSetOperationWire{Op: op, Target: target, Value: rawValue})
	case "concat":
		keys, err := frozenRequiredStrings(operation, "keys")
		if err != nil {
			return nil, err
		}
		separator, err := frozenOptionalString(operation, "separator")
		if err != nil {
			return nil, err
		}
		wireOperation := frozenConcatOperationWire{Op: op, Target: target, Keys: *keys}
		if separator != nil {
			wireOperation.Separator = *separator
		}
		return marshalFrozenWire(wireOperation)
	case "copy":
		source, err := frozenRequiredString(operation, "source")
		if err != nil {
			return nil, err
		}
		return marshalFrozenWire(frozenCopyOperationWire{Op: op, Target: target, Source: source})
	}
	// Unreachable: OperationFor above already rejects unknown ops. Kept so
	// the function stays total without a default branch.
	return nil, fmt.Errorf("unknown transform operation %q", op)
}

// requireStepSchemaKeys rejects config keys outside the shared declarative
// schema (internal/declarative/schema), so the freeze-time allowed set is
// the same single source the write-time validator consumes.
func requireStepSchemaKeys(config map[string]any, stepType string) error {
	step, ok := schema.StepFor(stepType)
	if !ok {
		return nil
	}
	return requireAllowedFrozenKeys(config, step.AllowedKeys()...)
}

func decodeFrozenFactoryInput(raw json.RawMessage) (registry.GraphDefinition, error) {
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return registry.GraphDefinition{}, fmt.Errorf("decode factory_input: %w", err)
	}
	if err := rejectFrozenNulls(canonical); err != nil {
		return registry.GraphDefinition{}, err
	}
	var input frozenFactoryInputDecodeWire
	if err := decodeFrozenStrict(canonical, &input); err != nil {
		return registry.GraphDefinition{}, err
	}
	if input.Entry == nil || *input.Entry == "" {
		return registry.GraphDefinition{}, fmt.Errorf("entry is required")
	}
	if len(input.Steps) == 0 {
		return registry.GraphDefinition{}, fmt.Errorf("steps must be a non-empty array")
	}
	definition := registry.GraphDefinition{
		Entry: *input.Entry,
		Steps: make([]registry.StepDefinition, len(input.Steps)),
	}
	for index, rawStep := range input.Steps {
		step, err := decodeFrozenStep(rawStep)
		if err != nil {
			return registry.GraphDefinition{}, fmt.Errorf("step %d: %w", index, err)
		}
		definition.Steps[index] = step
	}
	if err := definition.Validate(); err != nil {
		return registry.GraphDefinition{}, err
	}
	return definition, nil
}

func decodeFrozenStep(raw json.RawMessage) (registry.StepDefinition, error) {
	var wire frozenStepDecodeWire
	if err := decodeFrozenStrict(raw, &wire); err != nil {
		return registry.StepDefinition{}, err
	}
	if wire.Name == nil || *wire.Name == "" {
		return registry.StepDefinition{}, fmt.Errorf("name is required")
	}
	if wire.Type == nil || *wire.Type == "" {
		return registry.StepDefinition{}, fmt.Errorf("type is required")
	}
	if wire.Display == nil {
		return registry.StepDefinition{}, fmt.Errorf("display is required")
	}
	if len(wire.Config) == 0 {
		return registry.StepDefinition{}, fmt.Errorf("config is required")
	}
	if wire.Next != nil && wire.Condition != nil {
		return registry.StepDefinition{}, fmt.Errorf("next and condition are mutually exclusive")
	}
	if wire.Condition != nil {
		if wire.Condition.Key == nil || *wire.Condition.Key == "" ||
			wire.Condition.TrueStep == nil || wire.Condition.FalseStep == nil {
			return registry.StepDefinition{}, fmt.Errorf("condition key, true, and false are required")
		}
	}
	config, err := decodeFrozenConfig(*wire.Type, wire.Config)
	if err != nil {
		return registry.StepDefinition{}, err
	}
	step := registry.StepDefinition{
		Name: *wire.Name, Type: *wire.Type, Display: *wire.Display,
		Next: wire.Next, Config: config,
	}
	if wire.Condition != nil {
		step.Condition = &registry.ConditionDef{
			Key:       *wire.Condition.Key,
			TrueStep:  cloneStringPointer(wire.Condition.TrueStep),
			FalseStep: cloneStringPointer(wire.Condition.FalseStep),
		}
	}
	return step, nil
}

func decodeFrozenConfig(stepType string, raw json.RawMessage) (map[string]any, error) {
	config := make(map[string]any)
	switch stepType {
	case "chat":
		var wire frozenChatConfigWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		putFrozenString(config, "model", wire.Model)
		putFrozenString(config, "system_prompt", wire.SystemPrompt)
		putFrozenInt(config, "max_iterations", wire.MaxIterations)
		putFrozenInt(config, "max_loops", wire.MaxLoops)
	case "llm_call":
		var wire frozenLLMCallConfigWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		putFrozenString(config, "model", wire.Model)
		putFrozenString(config, "prompt_template", wire.PromptTemplate)
		putFrozenStrings(config, "input_keys", wire.InputKeys)
		putFrozenString(config, "output_key", wire.OutputKey)
		if wire.Stream != nil {
			config["stream"] = *wire.Stream
		}
		putFrozenInt(config, "max_loops", wire.MaxLoops)
		if wire.Extract != nil {
			extract := make(map[string]any)
			putFrozenString(extract, "key", wire.Extract.Key)
			putFrozenString(extract, "mode", wire.Extract.Mode)
			putFrozenStrings(extract, "keywords_true", wire.Extract.KeywordsTrue)
			config["extract"] = extract
		}
	case "llm_check":
		var wire frozenLLMCheckConfigWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		putFrozenString(config, "model", wire.Model)
		putFrozenString(config, "prompt_template", wire.PromptTemplate)
		putFrozenStrings(config, "input_keys", wire.InputKeys)
		putFrozenString(config, "output_key", wire.OutputKey)
		putFrozenString(config, "extract_mode", wire.ExtractMode)
		putFrozenString(config, "json_field", wire.JSONField)
		putFrozenStrings(config, "keywords_true", wire.KeywordsTrue)
		putFrozenInt(config, "max_loops", wire.MaxLoops)
	case "yield":
		var wire frozenYieldConfigWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		putFrozenString(config, "yield_type", wire.YieldType)
		putFrozenInt(config, "max_loops", wire.MaxLoops)
	case "transform":
		var wire frozenTransformConfigWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		if wire.Operations != nil {
			operations := make([]any, len(wire.Operations))
			for index, rawOperation := range wire.Operations {
				operation, err := decodeFrozenOperation(rawOperation)
				if err != nil {
					return nil, fmt.Errorf("operation %d: %w", index, err)
				}
				operations[index] = operation
			}
			config["operations"] = operations
		}
		putFrozenInt(config, "max_loops", wire.MaxLoops)
	case "builtin":
		var wire frozenBuiltinConfigWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		if wire.BuiltinType == nil {
			return nil, fmt.Errorf("builtin_type is required")
		}
		switch *wire.BuiltinType {
		case "guard", "prompt_assemble", "memory_retrieve":
		default:
			return nil, fmt.Errorf("unknown builtin type %q", *wire.BuiltinType)
		}
		putFrozenString(config, "builtin_type", wire.BuiltinType)
		putFrozenInt(config, "max_loops", wire.MaxLoops)
	case "worker":
		return nil, fmt.Errorf("worker steps are not supported by declarative frozen v1")
	default:
		return nil, fmt.Errorf("unknown declarative step type %q", stepType)
	}
	return config, nil
}

func decodeFrozenOperation(raw json.RawMessage) (map[string]any, error) {
	var tag frozenOperationTagWire
	if err := json.Unmarshal(raw, &tag); err != nil {
		return nil, err
	}
	operation := make(map[string]any)
	switch tag.Op {
	case "set":
		var wire frozenSetOperationWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		if wire.Target == "" || len(wire.Value) == 0 {
			return nil, fmt.Errorf("set target and value are required")
		}
		var value any
		if err := json.Unmarshal(wire.Value, &value); err != nil {
			return nil, err
		}
		operation["op"], operation["target"], operation["value"] = wire.Op, wire.Target, value
	case "concat":
		var wire frozenConcatOperationWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		if wire.Target == "" || wire.Keys == nil {
			return nil, fmt.Errorf("concat target and keys are required")
		}
		keys := make([]any, len(wire.Keys))
		for index := range wire.Keys {
			keys[index] = wire.Keys[index]
		}
		operation["op"], operation["target"] = wire.Op, wire.Target
		operation["keys"], operation["separator"] = keys, wire.Separator
	case "copy":
		var wire frozenCopyOperationWire
		if err := decodeFrozenStrict(raw, &wire); err != nil {
			return nil, err
		}
		if wire.Target == "" || wire.Source == "" {
			return nil, fmt.Errorf("copy target and source are required")
		}
		operation["op"], operation["target"], operation["source"] = wire.Op, wire.Target, wire.Source
	default:
		return nil, fmt.Errorf("unknown transform operation %q", tag.Op)
	}
	return operation, nil
}

func bindFrozenModels(definition *registry.GraphDefinition, bundle frozen.FrozenExecutionBundle) error {
	for index := range definition.Steps {
		step := &definition.Steps[index]
		switch step.Type {
		case "chat", "llm_call", "llm_check":
			model, _ := step.Config["model"].(string)
			if model == "" {
				model = bundle.PrimaryModel.ModelID
			}
			if model == "" || !frozenModelAllowed(model, bundle) {
				return fmt.Errorf("step %q model %q is not present in the frozen model bundle", step.Name, model)
			}
			step.Config["model"] = model
		}
	}
	return nil
}

func frozenModelAllowed(model string, bundle frozen.FrozenExecutionBundle) bool {
	if model == bundle.PrimaryModel.ModelID {
		return true
	}
	for _, fallback := range bundle.FallbackModels {
		if model == fallback.ModelID {
			return true
		}
	}
	return false
}

func validateFrozenRuntimeDependencies(definition registry.GraphDefinition, opts compiler.FrozenBuildOpts) error {
	for _, step := range definition.Steps {
		switch step.Type {
		case "chat":
			if frozenNilValue(opts.LLM) {
				return fmt.Errorf("step %q requires a non-nil LLM", step.Name)
			}
			if frozenNilValue(opts.Tools) {
				return fmt.Errorf("step %q requires a non-nil tool dispatcher", step.Name)
			}
		case "llm_call", "llm_check":
			if frozenNilValue(opts.LLM) {
				return fmt.Errorf("step %q requires a non-nil LLM", step.Name)
			}
		}
	}
	return nil
}

func buildFrozenChatStep(config map[string]any, opts compiler.FrozenBuildOpts) loom.Step {
	return stdlib.NewToolLoopStep(opts.LLM, opts.Tools, stdlib.ToolLoopOpts{
		Model:         getStr(config, "model", ""),
		SystemPrompt:  getStr(config, "system_prompt", ""),
		MaxIterations: getInt(config, "max_iterations", 20),
		ToolHooks:     append([]contract.ToolHook(nil), opts.Hooks.ToolHooks...),
	})
}

func buildFrozenPromptAssembleStep(record *registry.AgentRecord) loom.Step {
	identity := record.Spec.Identity
	identity.Core = grounding.GroundIdentity(identity.Core)
	return stdlib.NewPromptAssembleStep(stdlib.PromptConfig{
		Identity:              identity,
		Skills:                append([]stdlib.SkillDef(nil), record.Spec.Skills...),
		ProfilesMap:           record.Spec.Profiles,
		MaxSystemPromptTokens: 8000,
		SkillMatcher:          &stdlib.KeywordMatcher{},
	})
}

func frozenRegistryRecord(bundle frozen.FrozenExecutionBundle) (registry.AgentRecord, error) {
	profiles := make(map[string]stdlib.ProfileEntry, len(bundle.Agent.Profiles))
	for name, profile := range bundle.Agent.Profiles {
		profiles[name] = stdlib.ProfileEntry{
			SystemAddition: profile.SystemAddition,
			Greeting:       profile.Greeting,
		}
	}
	skills := make([]stdlib.SkillDef, len(bundle.Skills))
	for index, skill := range bundle.Skills {
		if skill.SourceType == "builtin" && skill.Body == "" {
			return registry.AgentRecord{}, fmt.Errorf(
				"frozen builtin skill %q body is required",
				skill.Name,
			)
		}
		skills[index] = stdlib.SkillDef{
			Name: skill.Name, Description: skill.Description, Body: skill.Body,
			AlwaysActive: skill.AlwaysActive,
		}
	}
	record := registry.AgentRecord{
		Name: bundle.Agent.Name, WorkspaceID: bundle.Agent.WorkspaceID,
		Model: bundle.PrimaryModel.ModelID,
		Spec: stdlib.AgentSpec{
			SystemPrompt: bundle.Agent.SystemPrompt,
			Identity: stdlib.IdentitySpec{
				Core: bundle.Agent.Identity.Core, Extended: bundle.Agent.Identity.Extended,
				Raw: bundle.Agent.Identity.Raw,
			},
			Profiles: profiles,
			Skills:   skills,
		},
	}
	if bundle.Agent.Guard != nil {
		maxInputLen, err := frozenInt64ToInt(bundle.Agent.Guard.MaxInputLen)
		if err != nil {
			return registry.AgentRecord{}, err
		}
		record.Guard = &registry.GuardConfig{
			Enabled: bundle.Agent.Guard.Enabled, MaxInputLen: maxInputLen,
			BlockedTerms: append([]string(nil), bundle.Agent.Guard.BlockedTerms...),
		}
	}
	if bundle.Agent.MemoryConfig != nil {
		topK, err := frozenInt64ToInt(bundle.Agent.MemoryConfig.TopK)
		if err != nil {
			return registry.AgentRecord{}, err
		}
		record.MemoryConfig = &registry.MemoryConfig{
			Enabled: bundle.Agent.MemoryConfig.Enabled, TopK: topK,
			AutoRemember: bundle.Agent.MemoryConfig.AutoRemember,
			Scope:        bundle.Agent.MemoryConfig.Scope,
		}
	}
	return record, nil
}

func rejectFrozenNulls(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("factory_input must contain one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("decode factory_input: %w", err)
	}
	if err := requireFrozenJSONEOF(decoder); err != nil {
		return err
	}
	if frozenContainsNull(value) {
		return fmt.Errorf("factory_input must not contain null")
	}
	return nil
}

func frozenContainsNull(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case []any:
		for _, item := range typed {
			if frozenContainsNull(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if frozenContainsNull(item) {
				return true
			}
		}
	}
	return false
}

func decodeFrozenStrict(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode factory_input: %w", err)
	}
	return requireFrozenJSONEOF(decoder)
}

func requireFrozenJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errorsIsEOF(err) {
		if err == nil {
			return fmt.Errorf("factory_input must contain exactly one JSON value")
		}
		return fmt.Errorf("decode factory_input: %w", err)
	}
	return nil
}

func errorsIsEOF(err error) bool {
	return err == io.EOF
}

func frozenFactoryCompileError(cause error) error {
	return fmt.Errorf("%w: %v", compiler.ErrFactoryCompileFailed, cause)
}

func marshalFrozenWire[T any](wire T) (json.RawMessage, error) {
	raw, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode declarative wire value: %w", err)
	}
	return raw, nil
}

func requireAllowedFrozenKeys(config map[string]any, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range config {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}

func frozenOptionalString(config map[string]any, key string) (*string, error) {
	value, exists := config[key]
	if !exists {
		return nil, nil
	}
	typed, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("%s must be a string", key)
	}
	return stringPointer(typed), nil
}

func frozenRequiredString(config map[string]any, key string) (string, error) {
	value, err := frozenOptionalString(config, key)
	if err != nil {
		return "", err
	}
	if value == nil || *value == "" {
		return "", fmt.Errorf("%s must be a non-empty string", key)
	}
	return *value, nil
}

func frozenOptionalBool(config map[string]any, key string) (*bool, error) {
	value, exists := config[key]
	if !exists {
		return nil, nil
	}
	typed, ok := value.(bool)
	if !ok {
		return nil, fmt.Errorf("%s must be a boolean", key)
	}
	return &typed, nil
}

func frozenOptionalInt(config map[string]any, key string) (*int, error) {
	value, exists := config[key]
	if !exists {
		return nil, nil
	}
	var integer int64
	switch typed := value.(type) {
	case int:
		return &typed, nil
	case int8:
		integer = int64(typed)
	case int16:
		integer = int64(typed)
	case int32:
		integer = int64(typed)
	case int64:
		integer = typed
	case uint:
		if uint64(typed) > uint64(^uint(0)>>1) {
			return nil, fmt.Errorf("%s is outside int range", key)
		}
		result := int(typed)
		return &result, nil
	case uint8:
		integer = int64(typed)
	case uint16:
		integer = int64(typed)
	case uint32:
		integer = int64(typed)
	case uint64:
		if typed > uint64(^uint(0)>>1) {
			return nil, fmt.Errorf("%s is outside int range", key)
		}
		result := int(typed)
		return &result, nil
	case float64:
		integer = int64(typed)
		if float64(integer) != typed {
			return nil, fmt.Errorf("%s must be an integer", key)
		}
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return nil, fmt.Errorf("%s must be an integer", key)
		}
		integer = parsed
	default:
		return nil, fmt.Errorf("%s must be an integer", key)
	}
	result, err := frozenInt64ToInt(integer)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return &result, nil
}

func frozenOptionalStrings(config map[string]any, key string) (*[]string, error) {
	value, exists := config[key]
	if !exists {
		return nil, nil
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || reflected.Kind() != reflect.Slice {
		return nil, fmt.Errorf("%s must be an array of strings", key)
	}
	result := make([]string, reflected.Len())
	for index := 0; index < reflected.Len(); index++ {
		item, ok := reflected.Index(index).Interface().(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", key)
		}
		result[index] = item
	}
	return &result, nil
}

func frozenRequiredStrings(config map[string]any, key string) (*[]string, error) {
	value, err := frozenOptionalStrings(config, key)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func putFrozenString(config map[string]any, key string, value *string) {
	if value != nil {
		config[key] = *value
	}
}

func putFrozenInt(config map[string]any, key string, value *int) {
	if value != nil {
		config[key] = float64(*value)
	}
}

func putFrozenStrings(config map[string]any, key string, value *[]string) {
	if value == nil {
		return
	}
	items := make([]any, len(*value))
	for index := range *value {
		items[index] = (*value)[index]
	}
	config[key] = items
}

func frozenNilValue(value any) bool {
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

func frozenInt64ToInt(value int64) (int, error) {
	result := int(value)
	if int64(result) != value {
		return 0, fmt.Errorf("integer is outside int range")
	}
	return result, nil
}

func stringPointer(value string) *string {
	return &value
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	return stringPointer(*value)
}
