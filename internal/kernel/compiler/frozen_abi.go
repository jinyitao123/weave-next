package compiler

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/memory"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const (
	CodeFactoryDescriptorInvalid = "workflow_factory_descriptor_invalid"
	CodeFactoryDuplicate         = "workflow_factory_duplicate"
	CodeFactoryUnknown           = "workflow_factory_unknown"
	CodeFactoryAmbiguous         = "workflow_factory_ambiguous"
	CodeFactoryABIIncompatible   = "workflow_factory_abi_incompatible"
	CodeDependencyUnenumerable   = "workflow_dependency_unenumerable"

	maxCompilerErrorGraphNodes = 1024
	maxCompilerErrorGraphEdges = 4096
)

var (
	ErrFactoryDescriptorInvalid = &CompilerError{code: CodeFactoryDescriptorInvalid}
	ErrFactoryDuplicate         = &CompilerError{code: CodeFactoryDuplicate}
	ErrFactoryUnknown           = &CompilerError{code: CodeFactoryUnknown}
	ErrFactoryAmbiguous         = &CompilerError{code: CodeFactoryAmbiguous}
	ErrFactoryABIIncompatible   = &CompilerError{code: CodeFactoryABIIncompatible}
	ErrDependencyUnenumerable   = &CompilerError{code: CodeDependencyUnenumerable}
)

type CodedError interface {
	error
	Code() string
}

type CompilerError struct {
	code  string
	cause error
}

func (e *CompilerError) Error() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *CompilerError) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *CompilerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *CompilerError) Is(target error) bool {
	if e == nil {
		return false
	}
	targetCoded, ok := target.(CodedError)
	if !ok || isNilCompilerValue(targetCoded) {
		return false
	}
	targetCode := targetCoded.Code()
	return targetCode != "" && targetCode == e.code
}

func normalizeCompilerError(fallbackCode string, cause error) error {
	code := fallbackCode
	traversal := compilerErrorTraversal{
		states: make(map[error]compilerErrorVisitState),
	}
	if !traversal.collect(cause, 0) {
		return &CompilerError{code: fallbackCode}
	}
	for _, current := range traversal.chain {
		if coded, ok := current.(CodedError); ok {
			if sourceCode := coded.Code(); sourceCode != "" {
				code = sourceCode
				break
			}
		}
	}
	return &CompilerError{code: code, cause: cause}
}

type compilerErrorVisitState uint8

const (
	compilerErrorActive compilerErrorVisitState = iota + 1
	compilerErrorDone
)

type compilerErrorTraversal struct {
	states      map[error]compilerErrorVisitState
	chain       []error
	uniqueNodes int
	edges       int
}

func (t *compilerErrorTraversal) collect(current error, depth int) bool {
	if current == nil {
		return true
	}
	if depth > 100 || isNilCompilerValue(current) {
		return false
	}
	currentValue := reflect.ValueOf(current)
	if !currentValue.IsValid() || !currentValue.Comparable() {
		return false
	}
	switch t.states[current] {
	case compilerErrorActive:
		return false
	case compilerErrorDone:
		return true
	}
	t.uniqueNodes++
	if t.uniqueNodes > maxCompilerErrorGraphNodes {
		return false
	}
	t.states[current] = compilerErrorActive
	t.chain = append(t.chain, current)

	if many, ok := current.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) > maxCompilerErrorGraphEdges-t.edges {
			return false
		}
		t.edges += len(children)
		for _, child := range children {
			if !t.collect(child, depth+1) {
				return false
			}
		}
		t.states[current] = compilerErrorDone
		return true
	}
	if one, ok := current.(interface{ Unwrap() error }); ok {
		if t.edges == maxCompilerErrorGraphEdges {
			return false
		}
		t.edges++
		if !t.collect(one.Unwrap(), depth+1) {
			return false
		}
	}
	t.states[current] = compilerErrorDone
	return true
}

type FreezeSchema struct {
	SchemaID      string
	SchemaVersion int
}

type DependencyMetadata struct {
	Ref         frozen.EnumeratedDependencyRef
	Revision    *int64
	ContentHash string
}

type MetadataResolver interface {
	ResolveMetadata(context.Context, frozen.EnumeratedDependencyRef) (DependencyMetadata, error)
}

type FrozenResolver interface {
	Agent(context.Context, string, int64) (frozen.FrozenAgentRecord, error)
	Skill(context.Context, frozen.FrozenDependencyRef) (frozen.FrozenSkill, error)
	MCPBinding(context.Context, frozen.FrozenDependencyRef) (frozen.FrozenMCPBinding, error)
	ModelBinding(context.Context, frozen.FrozenDependencyRef) (frozen.FrozenModelBinding, error)
	RuntimeBinding(context.Context, frozen.FrozenDependencyRef) (frozen.FrozenRuntimeBinding, error)
	DeliveryTarget(context.Context, frozen.FrozenDependencyRef) (frozen.FrozenDeliveryTarget, error)
}

type CredentialRefEncoder interface {
	EncodeReference(context.Context, string, string, string) (frozen.CredentialReference, error)
}

type DependencyEnumerator interface {
	FreezeSchema() FreezeSchema
	EncodeFactoryInput(context.Context, registry.AgentRecord, CredentialRefEncoder) (json.RawMessage, error)
	EnumerateDependencies(context.Context, frozen.FrozenAgentRecord, MetadataResolver) (frozen.EnumeratedDependencyManifest, error)
}

type FrozenHookPoints struct {
	ToolHooks       []contract.ToolHook
	BeforeStepHooks []loom.StepHook
	AfterStepHooks  []loom.StepHook
}

type FrozenBuildOpts struct {
	LLM             contract.LLM
	Tools           contract.ToolDispatcher
	Hooks           FrozenHookPoints
	MemoryService   *memory.Service
	CheckpointStore loom.Store
	AuditStore      loom.Store
	// ExecutionLLMWrapper decorates the LLM the compiled graph executes with,
	// mirroring CompileOpts.ExecutionLLMWrapper so frozen standard graphs get
	// the same logical usage wrapper PreparedRun installs. Nil keeps the
	// previous behavior.
	ExecutionLLMWrapper func(contract.LLM) contract.LLM
}

type GraphFactoryDescriptor struct {
	FactoryID             string
	FactoryVersion        string
	CompilerABI           string
	EnumerateDependencies DependencyEnumerator
	DescribeCapability    func(context.Context, frozen.FrozenAgentRecord) (frozen.CapabilityManifest, error)
	Compile               func(context.Context, frozen.FrozenExecutionBundle, FrozenResolver, FrozenBuildOpts) (*loom.Graph, frozen.CapabilityManifest, error)
}

func (d GraphFactoryDescriptor) Key() frozen.FactoryKey {
	return frozen.FactoryKey{
		FactoryID:      d.FactoryID,
		FactoryVersion: d.FactoryVersion,
		CompilerABI:    d.CompilerABI,
	}
}

type DescriptorRegistry struct {
	mu          sync.RWMutex
	descriptors map[frozen.FactoryKey]GraphFactoryDescriptor
}

func NewDescriptorRegistry() *DescriptorRegistry {
	return &DescriptorRegistry{descriptors: make(map[frozen.FactoryKey]GraphFactoryDescriptor)}
}

func (r *DescriptorRegistry) Register(descriptor GraphFactoryDescriptor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := descriptor.Key()
	if key.FactoryID == "" ||
		key.FactoryVersion == "" ||
		key.CompilerABI == "" ||
		isNilCompilerValue(descriptor.EnumerateDependencies) ||
		descriptor.Compile == nil {
		return ErrFactoryDescriptorInvalid
	}
	if _, exists := r.descriptors[key]; exists {
		return ErrFactoryDuplicate
	}
	r.descriptors[key] = descriptor
	return nil
}

func (r *DescriptorRegistry) Lookup(key frozen.FactoryKey) (GraphFactoryDescriptor, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if descriptor, ok := r.descriptors[key]; ok {
		return descriptor, nil
	}
	for registeredKey := range r.descriptors {
		if registeredKey.FactoryID == key.FactoryID &&
			registeredKey.FactoryVersion == key.FactoryVersion {
			return GraphFactoryDescriptor{}, ErrFactoryABIIncompatible
		}
	}
	return GraphFactoryDescriptor{}, ErrFactoryUnknown
}

// SelectFactoryKey resolves a graph type only when exactly one full factory key matches.
func (r *DescriptorRegistry) SelectFactoryKey(graphType string) (frozen.FactoryKey, error) {
	if graphType == "" {
		graphType = "standard"
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var selected frozen.FactoryKey
	found := false
	for key := range r.descriptors {
		if key.FactoryID != graphType {
			continue
		}
		if found {
			return frozen.FactoryKey{}, ErrFactoryAmbiguous
		}
		selected = key
		found = true
	}
	if !found {
		return frozen.FactoryKey{}, ErrFactoryUnknown
	}
	return selected, nil
}

func isNilCompilerValue(value any) bool {
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

var defaultDescriptorRegistry = NewDescriptorRegistry()

func RegisterDescriptor(descriptor GraphFactoryDescriptor) error {
	return defaultDescriptorRegistry.Register(descriptor)
}
