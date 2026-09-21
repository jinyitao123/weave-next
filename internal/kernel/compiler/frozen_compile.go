package compiler

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"sync"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const (
	CodeFrozenDependencyUndeclared = "workflow_frozen_dependency_undeclared"
	CodeFrozenManifestMismatch     = "workflow_frozen_manifest_mismatch"
	CodeFrozenCapabilityMismatch   = "workflow_frozen_capability_mismatch"
	CodeFactoryCompileFailed       = "workflow_factory_compile_failed"
)

var (
	ErrFrozenDependencyUndeclared = &CompilerError{code: CodeFrozenDependencyUndeclared}
	ErrFrozenManifestMismatch     = &CompilerError{code: CodeFrozenManifestMismatch}
	ErrFrozenCapabilityMismatch   = &CompilerError{code: CodeFrozenCapabilityMismatch}
	ErrFactoryCompileFailed       = &CompilerError{code: CodeFactoryCompileFailed}
)

// CompileFrozen compiles a graph strictly from a validated frozen bundle.
func CompileFrozen(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	resolver FrozenResolver,
	opts FrozenBuildOpts,
) (*loom.Graph, error) {
	return CompileFrozenWithRegistry(ctx, defaultDescriptorRegistry, bundle, resolver, opts)
}

// CompileFrozenWithRegistry compiles a graph using only the supplied descriptor registry.
func CompileFrozenWithRegistry(
	ctx context.Context,
	registry *DescriptorRegistry,
	bundle frozen.FrozenExecutionBundle,
	resolver FrozenResolver,
	opts FrozenBuildOpts,
) (*loom.Graph, error) {
	graph, _, err := compileFrozenWithRegistry(ctx, registry, bundle, resolver, opts, frozenCompileMode{
		compareStoredCapability: true,
	})
	return graph, err
}

type frozenCompileMode struct {
	compareStoredCapability bool
	allowRuntimeDiscovery   bool
}

func compileFrozenWithRegistry(
	ctx context.Context,
	registry *DescriptorRegistry,
	bundle frozen.FrozenExecutionBundle,
	resolver FrozenResolver,
	opts FrozenBuildOpts,
	mode frozenCompileMode,
) (*loom.Graph, frozen.CapabilityManifest, error) {
	descriptor, err := registry.Lookup(bundle.FactoryKey)
	if err != nil {
		return nil, frozen.CapabilityManifest{}, err
	}
	if descriptor.Compile == nil {
		return nil, frozen.CapabilityManifest{}, ErrFactoryCompileFailed
	}
	if isNilCompilerValue(descriptor.EnumerateDependencies) {
		return nil, frozen.CapabilityManifest{}, ErrDependencyUnenumerable
	}
	if err := frozen.ValidateManifest(bundle.Dependencies); err != nil {
		return nil, frozen.CapabilityManifest{}, normalizeCompilerError(CodeFrozenManifestMismatch, err)
	}
	var storedCapability []byte
	if mode.compareStoredCapability {
		storedCapability, err = frozen.Canonicalize(bundle.Capability, frozen.PreorderCapabilityManifest)
		if err != nil {
			return nil, frozen.CapabilityManifest{}, normalizeCompilerError(CodeFrozenCapabilityMismatch, err)
		}
	}

	metadataBundle := cloneFrozenExecutionBundle(bundle)
	metadata := newManifestMetadataResolver(metadataBundle.Dependencies)
	metadata.allowRuntimeDiscovery = mode.allowRuntimeDiscovery
	enumerated, enumerateErr := descriptor.EnumerateDependencies.EnumerateDependencies(ctx, metadataBundle.Agent, metadata)
	metadata.close()
	if violation := metadata.err(); violation != nil {
		return nil, frozen.CapabilityManifest{}, violation
	}
	if enumerateErr != nil {
		return nil, frozen.CapabilityManifest{}, normalizeCompilerError(CodeDependencyUnenumerable, enumerateErr)
	}
	if err := compareEnumeratedManifest(metadata.accessedManifest(bundle.Dependencies.SchemaVersion), enumerated); err != nil {
		return nil, frozen.CapabilityManifest{}, err
	}

	guard := newManifestFrozenResolver(cloneFrozenExecutionBundle(bundle), resolver)
	graph, returnedCapability, compileErr := descriptor.Compile(ctx, cloneFrozenExecutionBundle(bundle), guard, opts)
	guard.close()
	if violation := guard.err(); violation != nil {
		return nil, frozen.CapabilityManifest{}, violation
	}
	if compileErr != nil {
		return nil, frozen.CapabilityManifest{}, normalizeCompilerError(CodeFactoryCompileFailed, compileErr)
	}
	if graph == nil {
		return nil, frozen.CapabilityManifest{}, ErrFactoryCompileFailed
	}
	returnedCapability, err = bindCapabilityIdentity(bundle.Agent, returnedCapability)
	if err != nil {
		return nil, frozen.CapabilityManifest{}, normalizeCompilerError(CodeFrozenCapabilityMismatch, err)
	}
	returnedCanonical, err := frozen.Canonicalize(returnedCapability, frozen.PreorderCapabilityManifest)
	if err != nil {
		return nil, frozen.CapabilityManifest{}, normalizeCompilerError(CodeFrozenCapabilityMismatch, err)
	}
	if mode.compareStoredCapability && !bytes.Equal(storedCapability, returnedCanonical) {
		return nil, frozen.CapabilityManifest{}, ErrFrozenCapabilityMismatch
	}
	return graph, returnedCapability, nil
}

// CompileCandidate compiles with isolated hosts and fresh in-memory stores.
func (r *DescriptorRegistry) CompileCandidate(
	ctx context.Context,
	key frozen.FactoryKey,
	bundle frozen.FrozenExecutionBundle,
	resolver FrozenResolver,
) (*loom.Graph, error) {
	graph, _, err := r.compileCandidate(ctx, key, bundle, resolver, true)
	return graph, err
}

// ProduceCandidateCapability compiles an unpublished bundle and returns the
// capability produced by its descriptor without comparing a stored value.
func (r *DescriptorRegistry) ProduceCandidateCapability(
	ctx context.Context,
	key frozen.FactoryKey,
	bundle frozen.FrozenExecutionBundle,
	resolver FrozenResolver,
) (*loom.Graph, frozen.CapabilityManifest, error) {
	return r.compileCandidate(ctx, key, bundle, resolver, false)
}

func (r *DescriptorRegistry) compileCandidate(
	ctx context.Context,
	key frozen.FactoryKey,
	bundle frozen.FrozenExecutionBundle,
	resolver FrozenResolver,
	compareStoredCapability bool,
) (*loom.Graph, frozen.CapabilityManifest, error) {
	bundle.FactoryKey = key
	llm := &NoOpLLM{compileTimeHostViolation: &compileTimeHostViolation{}}
	tools := &CompileGuardDispatcher{compileTimeHostViolation: &compileTimeHostViolation{}}
	graph, capability, err := compileFrozenWithRegistry(
		ctx,
		r,
		bundle,
		resolver,
		FrozenBuildOpts{
			LLM:             llm,
			Tools:           tools,
			CheckpointStore: loom.NewMemStore(),
			AuditStore:      loom.NewMemStore(),
		},
		frozenCompileMode{
			compareStoredCapability: compareStoredCapability,
			allowRuntimeDiscovery:   true,
		},
	)
	if hostErr := llm.compileTimeHostViolation.err(); hostErr != nil {
		return nil, frozen.CapabilityManifest{}, hostErr
	}
	if hostErr := tools.compileTimeHostViolation.err(); hostErr != nil {
		return nil, frozen.CapabilityManifest{}, hostErr
	}
	return graph, capability, err
}

func compareEnumeratedManifest(
	expected frozen.EnumeratedDependencyManifest,
	returned frozen.EnumeratedDependencyManifest,
) error {
	want, err := frozen.Canonicalize(expected, frozen.PreorderEnumeratedDependencyManifest)
	if err != nil {
		return normalizeCompilerError(CodeFrozenManifestMismatch, err)
	}
	got, err := frozen.Canonicalize(returned, frozen.PreorderEnumeratedDependencyManifest)
	if err != nil {
		return normalizeCompilerError(CodeFrozenManifestMismatch, err)
	}
	if !bytes.Equal(want, got) {
		return ErrFrozenManifestMismatch
	}
	return nil
}

type manifestMetadataResolver struct {
	mu                    sync.Mutex
	dependencies          []frozen.FrozenDependencyRef
	accessed              []frozen.EnumeratedDependencyRef
	allowRuntimeDiscovery bool
	closed                bool
	sticky                error
}

func newManifestMetadataResolver(manifest frozen.FrozenDependencyManifest) *manifestMetadataResolver {
	return &manifestMetadataResolver{dependencies: manifest.Dependencies}
}

func (r *manifestMetadataResolver) ResolveMetadata(
	_ context.Context,
	reference frozen.EnumeratedDependencyRef,
) (DependencyMetadata, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		if reference.DependencyVersion == nil &&
			(reference.DependencyType == "model_binding" ||
				reference.DependencyType == "mcp_binding" ||
				reference.DependencyType == "runtime_binding" && r.allowRuntimeDiscovery) {
			var matched *frozen.FrozenDependencyRef
			for index := range r.dependencies {
				if !discoveryRefMatches(reference, r.dependencies[index].EnumeratedDependencyRef) {
					continue
				}
				if matched != nil {
					r.recordLocked(ErrFrozenManifestMismatch)
					return DependencyMetadata{}, ErrFrozenManifestMismatch
				}
				matched = &r.dependencies[index]
			}
			if matched != nil {
				resolvedRef := cloneEnumeratedDependencyRef(matched.EnumeratedDependencyRef)
				r.recordAccessLocked(resolvedRef)
				return DependencyMetadata{
					Ref: resolvedRef, Revision: cloneInt64(matched.DependencyVersion),
					ContentHash: matched.ContentHash,
				}, nil
			}
		}
		for index := range r.dependencies {
			dependency := r.dependencies[index]
			if enumeratedDependencyRefsEqual(reference, dependency.EnumeratedDependencyRef) {
				resolvedRef := cloneEnumeratedDependencyRef(dependency.EnumeratedDependencyRef)
				r.recordAccessLocked(resolvedRef)
				return DependencyMetadata{
					Ref: resolvedRef, Revision: cloneInt64(dependency.DependencyVersion),
					ContentHash: dependency.ContentHash,
				}, nil
			}
		}
	}
	r.recordLocked(ErrFrozenDependencyUndeclared)
	return DependencyMetadata{}, ErrFrozenDependencyUndeclared
}

// SkillRefs derives the agent's registry_version skill bindings from the
// stored frozen manifest, satisfying compiler.SkillRefSource so compile-time
// re-enumeration stays strictly inside the bundle and never touches a live
// store. Only exact skill dependencies belonging to the requested agent are
// returned, in manifest order.
func (r *manifestMetadataResolver) SkillRefs(
	_ context.Context,
	workspaceID, agentID string,
	agentVersion int64,
) ([]registry.SkillRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	refs := make([]registry.SkillRef, 0)
	for index := range r.dependencies {
		ref := r.dependencies[index].EnumeratedDependencyRef
		if ref.DependencyType != "skill" ||
			ref.WorkspaceID != workspaceID ||
			ref.OwnerType != "agent" ||
			ref.OwnerID != agentID ||
			ref.OwnerAgentVersion == nil ||
			*ref.OwnerAgentVersion != agentVersion {
			continue
		}
		prefix, skillID, ok := strings.Cut(ref.DependencyKey, ":")
		if !ok || prefix != "registry" || skillID == "" || ref.DependencyVersion == nil {
			continue
		}
		refs = append(refs, registry.SkillRef{
			SourceType:   registry.SourceTypeRegistryVersion,
			SkillID:      skillID,
			SkillVersion: cloneInt64(ref.DependencyVersion),
		})
	}
	return refs, nil
}

func (r *manifestMetadataResolver) InlineSkillNames(
	_ context.Context,
	workspaceID, agentID string,
	agentVersion int64,
) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0)
	for index := range r.dependencies {
		ref := r.dependencies[index].EnumeratedDependencyRef
		if ref.DependencyType != "skill" || ref.WorkspaceID != workspaceID ||
			ref.OwnerType != "agent" || ref.OwnerID != agentID ||
			ref.OwnerAgentVersion == nil || *ref.OwnerAgentVersion != agentVersion {
			continue
		}
		prefix, name, ok := strings.Cut(ref.DependencyKey, ":")
		if ok && prefix == "inline" && name != "" && ref.DependencyVersion == nil {
			names = append(names, name)
		}
	}
	return names, nil
}

func discoveryRefMatches(query, stored frozen.EnumeratedDependencyRef) bool {
	return query.DependencyVersion == nil &&
		query.DependencyType == stored.DependencyType &&
		query.WorkspaceID == stored.WorkspaceID &&
		query.OwnerType == stored.OwnerType &&
		query.OwnerID == stored.OwnerID &&
		int64ValuesEqual(query.OwnerAgentVersion, stored.OwnerAgentVersion) &&
		query.DependencyKey == stored.DependencyKey
}

func (r *manifestMetadataResolver) recordLocked(err error) {
	if r.sticky == nil {
		r.sticky = err
	}
}

func (r *manifestMetadataResolver) recordAccessLocked(reference frozen.EnumeratedDependencyRef) {
	for index := range r.accessed {
		if enumeratedDependencyRefsEqual(r.accessed[index], reference) {
			return
		}
	}
	r.accessed = append(r.accessed, cloneEnumeratedDependencyRef(reference))
}

func (r *manifestMetadataResolver) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
}

func (r *manifestMetadataResolver) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sticky
}

func (r *manifestMetadataResolver) accessedManifest(schemaVersion int) frozen.EnumeratedDependencyManifest {
	r.mu.Lock()
	defer r.mu.Unlock()
	accessed := make([]frozen.EnumeratedDependencyRef, len(r.accessed))
	for index := range r.accessed {
		accessed[index] = cloneEnumeratedDependencyRef(r.accessed[index])
	}
	return frozen.EnumeratedDependencyManifest{SchemaVersion: schemaVersion, Dependencies: accessed}
}

type manifestFrozenResolver struct {
	mu       sync.Mutex
	closed   bool
	sticky   error
	active   int
	cond     *sync.Cond
	bundle   frozen.FrozenExecutionBundle
	resolver FrozenResolver
}

func newManifestFrozenResolver(bundle frozen.FrozenExecutionBundle, resolver FrozenResolver) *manifestFrozenResolver {
	guard := &manifestFrozenResolver{bundle: bundle, resolver: resolver}
	guard.cond = sync.NewCond(&guard.mu)
	return guard
}

func (r *manifestFrozenResolver) Agent(_ context.Context, agentID string, version int64) (frozen.FrozenAgentRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && agentID == r.bundle.Agent.AgentID && version == r.bundle.Agent.AgentVersion {
		return cloneFrozenAgentRecord(r.bundle.Agent), nil
	}
	r.recordLocked(ErrFrozenDependencyUndeclared)
	return frozen.FrozenAgentRecord{}, ErrFrozenDependencyUndeclared
}

func (r *manifestFrozenResolver) Skill(ctx context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenSkill, error) {
	if err := r.authorize(ref, "skill"); err != nil {
		return frozen.FrozenSkill{}, err
	}
	defer r.finishForward()
	resolver, err := r.forwardResolver()
	if err != nil {
		return frozen.FrozenSkill{}, err
	}
	value, err := resolver.Skill(ctx, ref)
	if err != nil {
		return frozen.FrozenSkill{}, r.recordForwardError(err)
	}
	return value, nil
}

func (r *manifestFrozenResolver) MCPBinding(ctx context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenMCPBinding, error) {
	if err := r.authorize(ref, "mcp_binding"); err != nil {
		return frozen.FrozenMCPBinding{}, err
	}
	defer r.finishForward()
	resolver, err := r.forwardResolver()
	if err != nil {
		return frozen.FrozenMCPBinding{}, err
	}
	value, err := resolver.MCPBinding(ctx, ref)
	if err != nil {
		return frozen.FrozenMCPBinding{}, r.recordForwardError(err)
	}
	return value, nil
}

func (r *manifestFrozenResolver) ModelBinding(ctx context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenModelBinding, error) {
	if err := r.authorize(ref, "model_binding"); err != nil {
		return frozen.FrozenModelBinding{}, err
	}
	defer r.finishForward()
	resolver, err := r.forwardResolver()
	if err != nil {
		return frozen.FrozenModelBinding{}, err
	}
	value, err := resolver.ModelBinding(ctx, ref)
	if err != nil {
		return frozen.FrozenModelBinding{}, r.recordForwardError(err)
	}
	return value, nil
}

func (r *manifestFrozenResolver) RuntimeBinding(ctx context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenRuntimeBinding, error) {
	if err := r.authorize(ref, "runtime_binding"); err != nil {
		return frozen.FrozenRuntimeBinding{}, err
	}
	defer r.finishForward()
	resolver, err := r.forwardResolver()
	if err != nil {
		return frozen.FrozenRuntimeBinding{}, err
	}
	value, err := resolver.RuntimeBinding(ctx, ref)
	if err != nil {
		return frozen.FrozenRuntimeBinding{}, r.recordForwardError(err)
	}
	return value, nil
}

func (r *manifestFrozenResolver) DeliveryTarget(ctx context.Context, ref frozen.FrozenDependencyRef) (frozen.FrozenDeliveryTarget, error) {
	if err := r.authorize(ref, "delivery_target"); err != nil {
		return frozen.FrozenDeliveryTarget{}, err
	}
	defer r.finishForward()
	resolver, err := r.forwardResolver()
	if err != nil {
		return frozen.FrozenDeliveryTarget{}, err
	}
	value, err := resolver.DeliveryTarget(ctx, ref)
	if err != nil {
		return frozen.FrozenDeliveryTarget{}, r.recordForwardError(err)
	}
	return value, nil
}

func (r *manifestFrozenResolver) recordLocked(err error) {
	if r.sticky == nil {
		r.sticky = err
	}
}

func (r *manifestFrozenResolver) authorize(ref frozen.FrozenDependencyRef, dependencyType string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed && ref.DependencyType == dependencyType {
		for index := range r.bundle.Dependencies.Dependencies {
			declared := r.bundle.Dependencies.Dependencies[index]
			if declared.ContentHash == ref.ContentHash &&
				enumeratedDependencyRefsEqual(declared.EnumeratedDependencyRef, ref.EnumeratedDependencyRef) {
				r.active++
				return nil
			}
		}
	}
	r.recordLocked(ErrFrozenDependencyUndeclared)
	return ErrFrozenDependencyUndeclared
}

func (r *manifestFrozenResolver) finishForward() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active--
	r.cond.Broadcast()
}

func (r *manifestFrozenResolver) forwardResolver() (FrozenResolver, error) {
	if isNilCompilerValue(r.resolver) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.recordLocked(ErrFactoryCompileFailed)
		return nil, ErrFactoryCompileFailed
	}
	return r.resolver, nil
}

func (r *manifestFrozenResolver) recordForwardError(cause error) error {
	normalized := normalizeCompilerError(CodeFactoryCompileFailed, cause)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recordLocked(normalized)
	return normalized
}

func (r *manifestFrozenResolver) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for r.active > 0 {
		r.cond.Wait()
	}
}

func (r *manifestFrozenResolver) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sticky
}

func enumeratedDependencyRefsEqual(left, right frozen.EnumeratedDependencyRef) bool {
	return left.WorkspaceID == right.WorkspaceID &&
		left.OwnerType == right.OwnerType &&
		left.OwnerID == right.OwnerID &&
		int64ValuesEqual(left.OwnerAgentVersion, right.OwnerAgentVersion) &&
		left.DependencyType == right.DependencyType &&
		left.DependencyKey == right.DependencyKey &&
		int64ValuesEqual(left.DependencyVersion, right.DependencyVersion)
}

func int64ValuesEqual(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneEnumeratedDependencyRef(reference frozen.EnumeratedDependencyRef) frozen.EnumeratedDependencyRef {
	reference.OwnerAgentVersion = cloneInt64(reference.OwnerAgentVersion)
	reference.DependencyVersion = cloneInt64(reference.DependencyVersion)
	return reference
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneFrozenExecutionBundle(bundle frozen.FrozenExecutionBundle) frozen.FrozenExecutionBundle {
	return cloneFrozenValue(bundle)
}

func cloneFrozenAgentRecord(agent frozen.FrozenAgentRecord) frozen.FrozenAgentRecord {
	return cloneFrozenValue(agent)
}

func cloneFrozenValue[T any](value T) T {
	return cloneFrozenReflectValue(reflect.ValueOf(value)).Interface().(T)
}

func cloneFrozenReflectValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneFrozenReflectValue(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.New(value.Type().Elem())
		result.Elem().Set(cloneFrozenReflectValue(value.Elem()))
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(cloneFrozenReflectValue(iterator.Key()), cloneFrozenReflectValue(iterator.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneFrozenReflectValue(value.Index(index)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneFrozenReflectValue(value.Index(index)))
		}
		return result
	case reflect.Struct:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.NumField(); index++ {
			result.Field(index).Set(cloneFrozenReflectValue(value.Field(index)))
		}
		return result
	default:
		return value
	}
}
