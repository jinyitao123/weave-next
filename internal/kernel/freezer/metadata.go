// Package freezer resolves publication dependencies through a caller-owned
// PostgreSQL transaction and returns secretless frozen DTOs.
package freezer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/delivery"
	"github.com/jinyitao123/weave/internal/kernel/mcpregistry"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/skills"
)

const (
	CodeDependencyVersionRequired    = "workflow_dependency_version_required"
	CodeDependencyUnenumerable       = "workflow_dependency_unenumerable"
	CodeSkillVersionRequired         = "workflow_skill_version_required"
	CodeProviderRevisionRequired     = "workflow_provider_revision_required"
	CodeCredentialUnavailable        = "workflow_credential_unavailable"
	CodeCredentialVersionUnsupported = "workflow_credential_version_unsupported"
	CodeFrozenManifestMismatch       = "workflow_frozen_manifest_mismatch"

	maxErrorGraphNodes = 1024
	maxErrorGraphEdges = 4096
	maxErrorGraphDepth = 100
)

var recognizedCodes = map[string]struct{}{
	CodeDependencyVersionRequired:    {},
	CodeDependencyUnenumerable:       {},
	CodeSkillVersionRequired:         {},
	CodeProviderRevisionRequired:     {},
	CodeCredentialUnavailable:        {},
	CodeCredentialVersionUnsupported: {},
	CodeFrozenManifestMismatch:       {},
}

// Error carries a stable workflow code while preserving an optional cause.
type Error struct {
	code  string
	cause error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.cause != nil && e.cause.Error() != "" {
		return e.code + ": " + e.cause.Error()
	}
	return e.code
}

func (e *Error) Code() string {
	if e == nil {
		return ""
	}
	return e.code
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *Error) Is(target error) bool {
	if e == nil || nilInterface(target) {
		return false
	}
	coded, ok := target.(interface{ Code() string })
	if !ok || nilInterface(coded) {
		return false
	}
	code := coded.Code()
	return code != "" && code == e.code
}

func newError(code string, cause error) error {
	if nilInterface(cause) {
		cause = nil
	}
	return &Error{code: code, cause: cause}
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Sources are the only stateful stores a Resolver may consult.
type Sources struct {
	Agents    FrozenAgentReader
	Skills    *skills.Store
	Providers *credentials.Store
	Delivery  *delivery.Store
}

// ResolvedDependency joins dependency metadata to exactly one typed frozen DTO.
type ResolvedDependency struct {
	Metadata       compiler.DependencyMetadata
	Agent          *frozen.FrozenAgentRecord
	Skill          *frozen.FrozenSkill
	MCPBinding     *frozen.FrozenMCPBinding
	ModelBinding   *frozen.FrozenModelBinding
	RuntimeBinding *frozen.FrozenRuntimeBinding
	DeliveryTarget *frozen.FrozenDeliveryTarget
}

// AgentUsage declares how one exact AgentVersion will be used in the
// publication bundle. It is intentionally independent of AgentRecord.Role.
type AgentUsage string

const (
	AgentUsageLead   AgentUsage = "lead"
	AgentUsageWorker AgentUsage = "worker"
)

type dependencyCacheKey struct {
	workspaceID, ownerType, ownerID string
	ownerVersionSet                 bool
	ownerVersion                    int64
	dependencyType, dependencyKey   string
	dependencyVersionSet            bool
	dependencyVersion               int64
}

type agentVersionKey struct {
	agentID string
	version int64
}

type agentCachePin struct {
	factoryKey   frozen.FactoryKey
	factoryInput string
}

// Resolver serializes access to pgx.Tx and caches immutable typed results.
type Resolver struct {
	mu                    sync.Mutex
	gate                  chan struct{}
	tx                    pgx.Tx
	workspaceID           string
	sources               Sources
	allowRuntimeDiscovery bool
	allowMCPDiscovery     bool
	cache                 map[dependencyCacheKey]ResolvedDependency
	agentPins             map[dependencyCacheKey]agentCachePin
	owners                map[agentVersionKey]registry.AgentRecord
	rosters               map[string][]frozen.FrozenTeamWorker
}

var _ compiler.MetadataResolver = (*Resolver)(nil)

// BeginFreeze creates a resolver around a transaction owned by its caller. It
// performs no source reads and never commits or rolls back tx.
func BeginFreeze(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	sources Sources,
) (*Resolver, error) {
	if ctx == nil || tx == nil || workspaceID == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		sources.Agents == nil || sources.Skills == nil ||
		sources.Providers == nil || sources.Delivery == nil {
		return nil, newError(CodeDependencyUnenumerable, nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolver := &Resolver{
		gate:                  newResolverGate(),
		tx:                    tx,
		workspaceID:           workspaceID,
		sources:               sources,
		allowRuntimeDiscovery: true,
		allowMCPDiscovery:     true,
		cache:                 make(map[dependencyCacheKey]ResolvedDependency),
		agentPins:             make(map[dependencyCacheKey]agentCachePin),
		owners:                make(map[agentVersionKey]registry.AgentRecord),
		rosters:               make(map[string][]frozen.FrozenTeamWorker),
	}
	return resolver, nil
}

// ResolveAgentVersion freezes an exact immutable AgentVersion with the caller's
// selected factory identity and encoded input.
func (r *Resolver) ResolveAgentVersion(
	ctx context.Context,
	ref frozen.EnumeratedDependencyRef,
	usage AgentUsage,
	key frozen.FactoryKey,
	factoryInput json.RawMessage,
) (ResolvedDependency, error) {
	if r == nil {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}
	if err := r.acquire(ctx); err != nil {
		return ResolvedDependency{}, err
	}
	defer r.release()
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := validateAgentSelfRef(ctx, r.workspaceID, ref, usage); err != nil {
		return ResolvedDependency{}, err
	}
	if err := r.validateRef(ctx, ref); err != nil {
		return ResolvedDependency{}, err
	}
	if ref.DependencyType != "agent" || ref.DependencyVersion == nil ||
		ref.DependencyKey == "" || key.FactoryID == "" ||
		key.FactoryVersion == "" || key.CompilerABI == "" {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}
	canonicalInput, err := frozen.CanonicalizeJSON(factoryInput)
	if err != nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, err)
	}
	if cached, ok := r.lookupLocked(ref); ok {
		pin, pinned := r.agentPins[cacheKey(ref)]
		if !pinned || pin.factoryKey != key ||
			pin.factoryInput != string(canonicalInput) {
			return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, errors.New("agent freeze request conflicts with its cached factory input"))
		}
		if usage == AgentUsageWorker {
			owner, exists := r.owners[agentVersionKey{
				agentID: ref.DependencyKey, version: *ref.DependencyVersion,
			}]
			if !exists {
				return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, nil)
			}
			if len(owner.SubAgents) != 0 || len(owner.Spec.SubAgents) != 0 {
				return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
			}
		}
		return cached, nil
	}
	if nilInterface(r.sources.Agents) || r.tx == nil {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}

	record, err := r.sources.Agents.ResolveAgentVersionTx(
		ctx, r.tx, r.workspaceID, ref.DependencyKey, ref.DependencyVersion,
	)
	if err != nil {
		return ResolvedDependency{}, classifySourceError(err, CodeDependencyVersionRequired, "resolve AgentVersion")
	}
	if record == nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, errors.New("agent version resolver returned no record"))
	}
	if usage == AgentUsageWorker &&
		(len(record.SubAgents) != 0 || len(record.Spec.SubAgents) != 0) {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}
	agent, err := freezeAgent(*record, key, canonicalInput)
	if err != nil {
		if errors.Is(err, newError(CodeDependencyUnenumerable, nil)) {
			return ResolvedDependency{}, newError(CodeDependencyUnenumerable, err)
		}
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, err)
	}
	hash, err := frozen.HashDTO(agent, frozen.PreorderFrozenAgentRecord)
	if err != nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, err)
	}
	resolved := ResolvedDependency{Metadata: metadataFor(ref, *ref.DependencyVersion, hash), Agent: &agent}
	r.storeLocked(ref, resolved)
	if r.agentPins == nil {
		r.agentPins = make(map[dependencyCacheKey]agentCachePin)
	}
	r.agentPins[cacheKey(ref)] = agentCachePin{
		factoryKey: key, factoryInput: string(canonicalInput),
	}
	owner, err := cloneAgentRecord(*record)
	if err != nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, err)
	}
	r.owners[agentVersionKey{agentID: record.ID, version: int64(record.Version)}] = owner
	return cloneResolved(resolved), nil
}

// ResolveTeamWorkersForShare freezes a complete roster while retaining the
// caller transaction's source locks.
func (r *Resolver) ResolveTeamWorkersForShare(
	ctx context.Context,
	teamID string,
) ([]frozen.FrozenTeamWorker, error) {
	if r == nil {
		return nil, newError(CodeDependencyUnenumerable, nil)
	}
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	defer r.release()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	if teamID == "" || teamID != strings.TrimSpace(teamID) || nilInterface(r.sources.Agents) || r.tx == nil {
		return nil, newError(CodeDependencyUnenumerable, nil)
	}
	if cached, ok := r.rosters[teamID]; ok {
		return cloneTeamWorkers(cached), nil
	}
	workers, err := r.sources.Agents.ResolveTeamWorkersForShareTx(ctx, r.tx, r.workspaceID, teamID)
	if err != nil {
		return nil, classifySourceError(err, CodeDependencyUnenumerable, "resolve TeamWorker roster")
	}
	frozenWorkers := make([]frozen.FrozenTeamWorker, len(workers))
	for index, worker := range workers {
		value := frozen.FrozenTeamWorker{
			SchemaVersion:      frozen.FrozenSchemaVersion,
			WorkspaceID:        worker.WorkspaceID,
			TeamID:             worker.TeamID,
			WorkerAgentID:      worker.WorkerAgentID,
			Duty:               worker.Duty,
			WhenToUse:          worker.WhenToUse,
			ContextInstruction: worker.ContextInstruction,
			AllowedKinds:       append([]string(nil), worker.AllowedKinds...),
			DefaultKind:        worker.DefaultKind,
			ResultRequirement:  worker.ResultRequirement,
			Enabled:            worker.Enabled,
		}
		normalized, normalizeErr := normalizeTeamWorker(value)
		if normalizeErr != nil || normalized.WorkspaceID != r.workspaceID || normalized.TeamID != teamID {
			return nil, newError(CodeFrozenManifestMismatch, normalizeErr)
		}
		frozenWorkers[index] = normalized
	}
	r.rosters[teamID] = cloneTeamWorkers(frozenWorkers)
	return cloneTeamWorkers(frozenWorkers), nil
}

// ResolveMetadata resolves or returns cached typed metadata for one enumerated
// dependency. Agent dependencies must first be frozen with ResolveAgentVersion.
func (r *Resolver) ResolveMetadata(
	ctx context.Context,
	ref frozen.EnumeratedDependencyRef,
) (compiler.DependencyMetadata, error) {
	if r == nil {
		return compiler.DependencyMetadata{}, newError(CodeDependencyUnenumerable, nil)
	}
	if err := r.acquire(ctx); err != nil {
		return compiler.DependencyMetadata{}, err
	}
	defer r.release()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateRef(ctx, ref); err != nil {
		return compiler.DependencyMetadata{}, err
	}
	if cached, ok := r.lookupLocked(ref); ok {
		return cloneMetadata(cached.Metadata), nil
	}

	var (
		resolved ResolvedDependency
		err      error
	)
	switch ref.DependencyType {
	case "agent":
		return compiler.DependencyMetadata{}, newError(CodeDependencyUnenumerable, nil)
	case "skill":
		resolved, err = r.resolveSkillLocked(ctx, ref)
	case "mcp_binding":
		resolved, err = r.resolveMCPLocked(ctx, ref)
	case "model_binding":
		resolved, err = r.resolveModelLocked(ctx, ref)
	case "runtime_binding":
		resolved, err = r.resolveRuntimeLocked(ctx, ref)
	case "delivery_target":
		resolved, err = r.resolveDeliveryLocked(ctx, ref)
	default:
		return compiler.DependencyMetadata{}, newError(CodeDependencyUnenumerable, nil)
	}
	if err != nil {
		return compiler.DependencyMetadata{}, err
	}
	return cloneMetadata(resolved.Metadata), nil
}

// Lookup returns a deep clone of a previously resolved dependency.
func (r *Resolver) Lookup(ref frozen.EnumeratedDependencyRef) (ResolvedDependency, bool) {
	if r == nil {
		return ResolvedDependency{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookupLocked(ref)
}

// SkillRefs returns the frozen agent's explicit skill bindings from the
// in-memory owner record, satisfying compiler.SkillRefSource for freeze-time
// enumeration. The agent must already have been frozen through
// ResolveAgentVersion; missing or cross-workspace owners fail closed.
func (r *Resolver) SkillRefs(
	ctx context.Context,
	workspaceID, agentID string,
	agentVersion int64,
) ([]registry.SkillRef, error) {
	if r == nil {
		return nil, newError(CodeDependencyUnenumerable, nil)
	}
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, ok := r.owners[agentVersionKey{agentID: agentID, version: agentVersion}]
	if !ok || owner.WorkspaceID != workspaceID {
		return nil, newError(CodeDependencyUnenumerable, nil)
	}
	return cloneSkillRefs(owner.SkillRefs), nil
}

func (r *Resolver) InlineSkillNames(
	ctx context.Context,
	workspaceID, agentID string,
	agentVersion int64,
) ([]string, error) {
	if r == nil {
		return nil, newError(CodeDependencyUnenumerable, nil)
	}
	if err := validateContext(ctx); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	owner, ok := r.owners[agentVersionKey{agentID: agentID, version: agentVersion}]
	if !ok || owner.WorkspaceID != workspaceID {
		return nil, newError(CodeDependencyUnenumerable, nil)
	}
	names := make([]string, 0, len(owner.Spec.Skills))
	for _, skill := range owner.Spec.Skills {
		names = append(names, skill.Name)
	}
	return names, nil
}

func (r *Resolver) resolveSkillLocked(ctx context.Context, ref frozen.EnumeratedDependencyRef) (ResolvedDependency, error) {
	prefix, name, ok := strings.Cut(ref.DependencyKey, ":")
	if !ok || name == "" || name != strings.TrimSpace(name) {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}

	var value frozen.FrozenSkill
	switch prefix {
	case "builtin", "inline":
		if ref.DependencyVersion != nil {
			return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
		}
		owner, ownerErr := r.ownerLocked(ref)
		if ownerErr != nil {
			return ResolvedDependency{}, ownerErr
		}
		declared, declaredErr := declaredSkill(owner, name)
		if declaredErr != nil {
			return ResolvedDependency{}, declaredErr
		}
		if len(declared.Scripts) != 0 || len(declared.References) != 0 {
			return ResolvedDependency{}, newError(CodeSkillVersionRequired, nil)
		}
		value = frozen.FrozenSkill{
			SchemaVersion: frozen.FrozenSchemaVersion,
			WorkspaceID:   r.workspaceID,
			Name:          name,
			SourceType:    prefix,
			Description:   declared.Description,
			AlwaysActive:  declared.AlwaysActive,
			Resources:     []frozen.FrozenSkillResource{},
		}
		if prefix == "builtin" {
			builtin, exists := compiler.BuiltinSkills[name]
			if !exists || builtin.Body == "" || declared.Body != "" {
				return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
			}
			value.Body = builtin.Body
			if value.Description == "" {
				value.Description = builtin.Description
			}
		} else {
			if declared.Body == "" {
				return ResolvedDependency{}, newError(CodeSkillVersionRequired, nil)
			}
			value.Body = declared.Body
		}
	case "registry":
		if ref.DependencyVersion == nil {
			return ResolvedDependency{}, newError(CodeDependencyVersionRequired, nil)
		}
		if r.sources.Skills == nil || r.tx == nil {
			return ResolvedDependency{}, newError(CodeSkillVersionRequired, nil)
		}
		stored, resolveErr := r.sources.Skills.ResolveSkillVersionTx(
			ctx, r.tx, r.workspaceID, name, ref.DependencyVersion,
		)
		if resolveErr != nil {
			return ResolvedDependency{}, classifySourceError(resolveErr, CodeSkillVersionRequired, "resolve SkillVersion")
		}
		if stored == nil || stored.SkillID != name || stored.WorkspaceID != r.workspaceID || stored.Version != *ref.DependencyVersion {
			return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, nil)
		}
		version := stored.Version
		value = frozen.FrozenSkill{
			SchemaVersion: frozen.FrozenSchemaVersion,
			WorkspaceID:   stored.WorkspaceID,
			Name:          stored.Name,
			SourceType:    "registry_version",
			SkillID:       stored.SkillID,
			SkillVersion:  &version,
			Description:   stored.Description,
			Body:          stored.Body,
			AlwaysActive:  stored.AlwaysActive,
			Resources:     append([]frozen.FrozenSkillResource(nil), stored.Resources...),
		}
	default:
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}

	normalized, err := normalizeSkill(value)
	if err != nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, err)
	}
	hash, err := frozen.HashDTO(normalized, frozen.PreorderFrozenSkill)
	if err != nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, err)
	}
	normalized.ContentHash = hash
	var metadata compiler.DependencyMetadata
	if ref.DependencyVersion == nil {
		metadata = unversionedMetadata(ref, hash)
	} else {
		metadata = metadataFor(ref, *ref.DependencyVersion, hash)
	}
	resolved := ResolvedDependency{Metadata: metadata, Skill: &normalized}
	r.storeLocked(ref, resolved)
	return cloneResolved(resolved), nil
}

func (r *Resolver) resolveMCPLocked(ctx context.Context, ref frozen.EnumeratedDependencyRef) (ResolvedDependency, error) {
	owner, err := r.ownerLocked(ref)
	if err != nil {
		return ResolvedDependency{}, err
	}
	var declaration *registry.MCPServerConfig
	for index := range owner.MCPServers {
		candidate := &owner.MCPServers[index]
		if candidate.ServerID != ref.DependencyKey {
			continue
		}
		if declaration != nil {
			return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
		}
		declaration = candidate
	}
	if declaration == nil || declaration.ServerID == "" || declaration.URL != "" || len(declaration.Headers) != 0 {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}
	policy := mcpregistry.MCPAgentPolicy{Filter: append([]string(nil), declaration.Filter...), WriteTools: append([]string(nil), declaration.WriteTools...)}
	var binding frozen.FrozenMCPBinding
	var resolveErr error
	if ref.DependencyVersion == nil {
		binding, resolveErr = mcpregistry.ResolveCurrentMCPToolsTx(ctx, r.tx, r.workspaceID, ref.DependencyKey, policy)
	} else {
		binding, resolveErr = mcpregistry.ResolveMCPRevisionTx(ctx, r.tx, r.workspaceID, ref.DependencyKey, ref.DependencyVersion, policy)
	}
	if resolveErr != nil {
		return ResolvedDependency{}, classifySourceError(resolveErr, CodeDependencyVersionRequired, "resolve MCP revision")
	}
	if binding.WorkspaceID != r.workspaceID || binding.ServerID != ref.DependencyKey ||
		(ref.DependencyVersion != nil && binding.ServerRevision != *ref.DependencyVersion) {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, nil)
	}
	resolvedRef := cloneRef(ref)
	resolvedRef.DependencyVersion = int64Pointer(binding.ServerRevision)
	resolved := ResolvedDependency{
		Metadata:   metadataFor(resolvedRef, binding.ServerRevision, binding.ContentHash),
		MCPBinding: &binding,
	}
	r.storeLocked(ref, resolved)
	r.storeLocked(resolvedRef, resolved)
	return cloneResolved(resolved), nil
}

func (r *Resolver) resolveModelLocked(ctx context.Context, ref frozen.EnumeratedDependencyRef) (ResolvedDependency, error) {
	if ref.OwnerType == "agent" {
		owner, err := r.ownerLocked(ref)
		if err != nil {
			return ResolvedDependency{}, err
		}
		if owner.Model != ref.DependencyKey && !slices.Contains(owner.FallbackModels, ref.DependencyKey) {
			return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
		}
	}
	binding, err := credentials.ResolveModelRevisionTx(ctx, r.tx, r.workspaceID, ref.DependencyKey)
	if err != nil {
		return ResolvedDependency{}, classifySourceError(err, CodeProviderRevisionRequired, "resolve model revision")
	}
	if binding.WorkspaceID != r.workspaceID || binding.ModelID != ref.DependencyKey {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, nil)
	}
	if ref.DependencyVersion != nil && *ref.DependencyVersion != binding.ProviderRevision {
		return ResolvedDependency{}, newError(CodeDependencyVersionRequired, nil)
	}
	resolvedRef := cloneRef(ref)
	resolvedRef.DependencyVersion = int64Pointer(binding.ProviderRevision)
	resolved := ResolvedDependency{
		Metadata:     metadataFor(resolvedRef, binding.ProviderRevision, binding.ContentHash),
		ModelBinding: &binding,
	}
	discoveryRef := cloneRef(resolvedRef)
	discoveryRef.DependencyVersion = nil
	r.storeLocked(discoveryRef, resolved)
	r.storeLocked(resolvedRef, resolved)
	return cloneResolved(resolved), nil
}

func (r *Resolver) resolveRuntimeLocked(ctx context.Context, ref frozen.EnumeratedDependencyRef) (ResolvedDependency, error) {
	owner, err := r.ownerLocked(ref)
	if err != nil {
		return ResolvedDependency{}, err
	}
	engine := owner.Engine
	if engine == "" {
		engine = "loom"
	}
	if engine == "loom" || owner.RuntimeID != ref.DependencyKey {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}
	var record runtimes.FrozenRuntimeRecord
	var resolveErr error
	if ref.DependencyVersion == nil {
		record, resolveErr = runtimes.ResolveCurrentRuntimeRevisionTx(
			ctx, r.tx, r.workspaceID, ref.DependencyKey,
		)
	} else {
		record, resolveErr = runtimes.ResolveRuntimeRevisionTx(
			ctx, r.tx, r.workspaceID, ref.DependencyKey, ref.DependencyVersion,
		)
	}
	if resolveErr != nil {
		return ResolvedDependency{}, classifySourceError(resolveErr, CodeDependencyVersionRequired, "resolve runtime revision")
	}
	resolvedRef := cloneRef(ref)
	resolvedRef.DependencyVersion = int64Pointer(record.RuntimeRevision)
	if record.WorkspaceID != r.workspaceID || record.RuntimeID != ref.DependencyKey ||
		(ref.DependencyVersion != nil && record.RuntimeRevision != *ref.DependencyVersion) ||
		!slices.Contains(record.Engines, engine) {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, nil)
	}
	binding := frozen.FrozenRuntimeBinding{
		SchemaVersion:   frozen.FrozenSchemaVersion,
		WorkspaceID:     record.WorkspaceID,
		RuntimeID:       record.RuntimeID,
		Engine:          engine,
		RuntimeRevision: record.RuntimeRevision,
		AccessRef:       record.AccessRef,
	}
	normalized, normalizeErr := normalizeRuntime(binding)
	if normalizeErr != nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, normalizeErr)
	}
	hash, hashErr := frozen.HashDTO(normalized, frozen.PreorderFrozenRuntimeBinding)
	if hashErr != nil {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, hashErr)
	}
	normalized.ContentHash = hash
	resolved := ResolvedDependency{
		Metadata:       metadataFor(resolvedRef, record.RuntimeRevision, hash),
		RuntimeBinding: &normalized,
	}
	discoveryRef := cloneRef(resolvedRef)
	discoveryRef.DependencyVersion = nil
	r.storeLocked(discoveryRef, resolved)
	r.storeLocked(resolvedRef, resolved)
	return cloneResolved(resolved), nil
}

func (r *Resolver) resolveDeliveryLocked(ctx context.Context, ref frozen.EnumeratedDependencyRef) (ResolvedDependency, error) {
	if r.sources.Delivery == nil || r.tx == nil {
		return ResolvedDependency{}, newError(CodeDependencyUnenumerable, nil)
	}
	target, err := r.sources.Delivery.ResolveDeliveryRevisionTx(
		ctx, r.tx, r.workspaceID, ref.DependencyKey, ref.DependencyVersion,
	)
	if err != nil {
		return ResolvedDependency{}, classifySourceError(err, CodeDependencyVersionRequired, "resolve delivery revision")
	}
	if target.WorkspaceID != r.workspaceID || target.TargetID != ref.DependencyKey || target.TargetRevision != *ref.DependencyVersion {
		return ResolvedDependency{}, newError(CodeFrozenManifestMismatch, nil)
	}
	resolved := ResolvedDependency{
		Metadata:       metadataFor(ref, target.TargetRevision, target.ContentHash),
		DeliveryTarget: &target,
	}
	r.storeLocked(ref, resolved)
	return cloneResolved(resolved), nil
}

func (r *Resolver) validateRef(ctx context.Context, ref frozen.EnumeratedDependencyRef) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if r.workspaceID == "" || ref.WorkspaceID != r.workspaceID ||
		ref.WorkspaceID != strings.TrimSpace(ref.WorkspaceID) ||
		ref.OwnerID == "" || ref.OwnerID != strings.TrimSpace(ref.OwnerID) ||
		ref.DependencyType == "" || ref.DependencyKey == "" ||
		ref.DependencyKey != strings.TrimSpace(ref.DependencyKey) {
		return newError(CodeDependencyUnenumerable, nil)
	}
	switch ref.OwnerType {
	case "agent":
		if ref.OwnerAgentVersion == nil {
			return newError(CodeDependencyVersionRequired, nil)
		}
		if !validVersion(*ref.OwnerAgentVersion) {
			return newError(CodeDependencyVersionRequired, nil)
		}
	case "workflow":
		if ref.OwnerAgentVersion != nil {
			return newError(CodeDependencyUnenumerable, nil)
		}
	default:
		return newError(CodeDependencyUnenumerable, nil)
	}
	if ref.DependencyVersion != nil && !validVersion(*ref.DependencyVersion) {
		return newError(CodeDependencyVersionRequired, nil)
	}
	switch ref.DependencyType {
	case "agent", "delivery_target":
		if ref.DependencyVersion == nil {
			return newError(CodeDependencyVersionRequired, nil)
		}
	case "mcp_binding":
		if ref.DependencyVersion == nil && !r.allowMCPDiscovery {
			return newError(CodeDependencyVersionRequired, nil)
		}
	case "skill":
		prefix, _, ok := strings.Cut(ref.DependencyKey, ":")
		if !ok || (prefix != "builtin" && prefix != "inline" && prefix != "registry") {
			return newError(CodeDependencyUnenumerable, nil)
		}
		if prefix == "registry" && ref.DependencyVersion == nil {
			return newError(CodeDependencyVersionRequired, nil)
		}
		if prefix != "registry" && ref.DependencyVersion != nil {
			return newError(CodeDependencyUnenumerable, nil)
		}
	case "model_binding":
	case "runtime_binding":
		if ref.DependencyVersion == nil && !r.allowRuntimeDiscovery {
			return newError(CodeDependencyVersionRequired, nil)
		}
	case "credential_reference", "factory":
		return newError(CodeDependencyUnenumerable, nil)
	default:
		return newError(CodeDependencyUnenumerable, nil)
	}
	return nil
}

func validateAgentSelfRef(
	ctx context.Context,
	workspaceID string,
	ref frozen.EnumeratedDependencyRef,
	usage AgentUsage,
) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if workspaceID == "" || ref.WorkspaceID != workspaceID ||
		usage != AgentUsageLead && usage != AgentUsageWorker ||
		ref.OwnerType != "agent" ||
		ref.OwnerID == "" || ref.OwnerID != ref.DependencyKey ||
		ref.DependencyType != "agent" ||
		ref.OwnerAgentVersion == nil || ref.DependencyVersion == nil ||
		!validVersion(*ref.OwnerAgentVersion) ||
		!validVersion(*ref.DependencyVersion) ||
		*ref.OwnerAgentVersion != *ref.DependencyVersion {
		return newError(CodeDependencyUnenumerable, nil)
	}
	return nil
}

func (r *Resolver) ownerLocked(ref frozen.EnumeratedDependencyRef) (registry.AgentRecord, error) {
	if ref.OwnerType != "agent" || ref.OwnerAgentVersion == nil {
		return registry.AgentRecord{}, newError(CodeDependencyUnenumerable, nil)
	}
	owner, ok := r.owners[agentVersionKey{agentID: ref.OwnerID, version: *ref.OwnerAgentVersion}]
	if !ok {
		return registry.AgentRecord{}, newError(CodeDependencyUnenumerable, nil)
	}
	cloned, err := cloneAgentRecord(owner)
	if err != nil {
		return registry.AgentRecord{}, newError(CodeFrozenManifestMismatch, err)
	}
	return cloned, nil
}

func freezeAgent(record registry.AgentRecord, key frozen.FactoryKey, input json.RawMessage) (frozen.FrozenAgentRecord, error) {
	profiles := make(map[string]frozen.FrozenAgentProfile, len(record.Spec.Profiles))
	for name, profile := range record.Spec.Profiles {
		profiles[name] = frozen.FrozenAgentProfile{
			SystemAddition: profile.SystemAddition,
			Greeting:       profile.Greeting,
		}
	}
	value := frozen.FrozenAgentRecord{
		SchemaVersion: frozen.FrozenSchemaVersion,
		WorkspaceID:   record.WorkspaceID,
		AgentID:       record.ID,
		AgentVersion:  int64(record.Version),
		Name:          record.Name,
		DisplayName:   record.DisplayName,
		Role:          record.Role,
		Engine:        record.Engine,
		RuntimeID:     record.RuntimeID,
		Model:         record.Model,
		SystemPrompt:  record.Spec.SystemPrompt,
		Identity: frozen.FrozenAgentIdentity{
			Core:     record.Spec.Identity.Core,
			Extended: record.Spec.Identity.Extended,
			Raw:      record.Spec.Identity.Raw,
		},
		Profiles: profiles,
		Permissions: frozen.FrozenPermissions{
			Deny:  append([]string(nil), record.Permissions.Deny...),
			Allow: append([]string(nil), record.Permissions.Allow...),
			Ask:   append([]string(nil), record.Permissions.Ask...),
		},
		MemorySlots:  make([]frozen.FrozenMemorySlot, len(record.MemorySlots)),
		OutputSchema: json.RawMessage("null"),
		Limits: frozen.FrozenAgentLimits{
			MaxCostUSD:      record.MaxCostUSD,
			MaxTokens:       record.MaxTokens,
			MaxOutputTokens: int64(record.MaxOutputTokens),
			StepBudget:      record.StepBudget,
			ToolLoopControl: record.ToolLoopControl,
			MaxToolRepeats:  int64(record.MaxToolRepeats),
		},
		Fallback: frozen.FrozenFallback{
			Models:  append([]string(nil), record.FallbackModels...),
			Retries: int64(record.FallbackRetries),
		},
		GraphType:    record.GraphType,
		FactoryInput: append(json.RawMessage(nil), input...),
		BusinessCapabilityIDs: append(
			[]string(nil),
			record.BusinessCapabilityIDs...,
		),
		BusinessCapabilityBindings: append([]frozen.BusinessCapabilityBinding(nil), record.BusinessCapabilityBindings...),
	}
	if value.GraphType == "" {
		value.GraphType = record.Spec.GraphType
	}
	if record.OutputSchema != nil {
		value.OutputSchema = append(json.RawMessage(nil), (*record.OutputSchema)...)
	}
	if record.MemoryConfig != nil {
		value.MemoryConfig = &frozen.FrozenMemoryConfig{
			Enabled:      record.MemoryConfig.Enabled,
			TopK:         int64(record.MemoryConfig.TopK),
			AutoRemember: record.MemoryConfig.AutoRemember,
			Scope:        record.MemoryConfig.Scope,
		}
	}
	for index, slot := range record.MemorySlots {
		value.MemorySlots[index] = frozen.FrozenMemorySlot{
			Key: slot.Key, Label: slot.Label, Description: slot.Description,
		}
	}
	if record.Guard != nil {
		value.Guard = &frozen.FrozenGuard{
			Enabled:      record.Guard.Enabled,
			MaxInputLen:  int64(record.Guard.MaxInputLen),
			BlockedTerms: append([]string(nil), record.Guard.BlockedTerms...),
		}
	}
	if record.Compaction != nil {
		value.Compaction = &frozen.FrozenCompaction{
			Enabled:        record.Compaction.Enabled,
			TokenThreshold: int64(record.Compaction.TokenThreshold),
		}
	}
	normalized, err := frozen.NormalizeFrozenAgentRecord(value)
	if err != nil {
		return frozen.FrozenAgentRecord{}, err
	}
	if normalized.Engine == "loom" && normalized.RuntimeID != "" ||
		normalized.Engine != "loom" && normalized.RuntimeID == "" {
		return frozen.FrozenAgentRecord{}, newError(CodeDependencyUnenumerable, nil)
	}
	if normalized.GraphType != key.FactoryID {
		return frozen.FrozenAgentRecord{}, errors.New("Agent graph type does not match factory identity")
	}
	return normalized, nil
}

func declaredSkill(record registry.AgentRecord, name string) (skillValue, error) {
	var found *skillValue
	for _, candidate := range record.Spec.Skills {
		if candidate.Name != name {
			continue
		}
		if found != nil {
			return skillValue{}, newError(CodeDependencyUnenumerable, nil)
		}
		copy := skillValue{
			Name: candidate.Name, Description: candidate.Description, Body: candidate.Body,
			AlwaysActive: candidate.AlwaysActive,
			Scripts:      append([]string(nil), candidate.Scripts...),
			References:   append([]string(nil), candidate.References...),
		}
		found = &copy
	}
	if found == nil {
		return skillValue{}, newError(CodeDependencyUnenumerable, nil)
	}
	return *found, nil
}

type skillValue struct {
	Name, Description, Body string
	AlwaysActive            bool
	Scripts, References     []string
}

func normalizeSkill(value frozen.FrozenSkill) (frozen.FrozenSkill, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return frozen.FrozenSkill{}, err
	}
	return frozen.DecodeFrozenSkill(raw)
}

func normalizeRuntime(value frozen.FrozenRuntimeBinding) (frozen.FrozenRuntimeBinding, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return frozen.FrozenRuntimeBinding{}, err
	}
	return frozen.DecodeFrozenRuntimeBinding(raw)
}

func normalizeTeamWorker(value frozen.FrozenTeamWorker) (frozen.FrozenTeamWorker, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return frozen.FrozenTeamWorker{}, err
	}
	return frozen.DecodeFrozenTeamWorker(raw)
}

func (r *Resolver) lookupLocked(ref frozen.EnumeratedDependencyRef) (ResolvedDependency, bool) {
	value, ok := r.cache[cacheKey(ref)]
	if !ok {
		return ResolvedDependency{}, false
	}
	return cloneResolved(value), true
}

func (r *Resolver) storeLocked(ref frozen.EnumeratedDependencyRef, value ResolvedDependency) {
	if r.cache == nil {
		r.cache = make(map[dependencyCacheKey]ResolvedDependency)
	}
	r.cache[cacheKey(ref)] = cloneResolved(value)
}

func cacheKey(ref frozen.EnumeratedDependencyRef) dependencyCacheKey {
	key := dependencyCacheKey{
		workspaceID: ref.WorkspaceID, ownerType: ref.OwnerType, ownerID: ref.OwnerID,
		dependencyType: ref.DependencyType, dependencyKey: ref.DependencyKey,
	}
	if ref.OwnerAgentVersion != nil {
		key.ownerVersionSet = true
		key.ownerVersion = *ref.OwnerAgentVersion
	}
	if ref.DependencyVersion != nil {
		key.dependencyVersionSet = true
		key.dependencyVersion = *ref.DependencyVersion
	}
	return key
}

func metadataFor(ref frozen.EnumeratedDependencyRef, revision int64, hash string) compiler.DependencyMetadata {
	exact := cloneRef(ref)
	exact.DependencyVersion = int64Pointer(revision)
	return compiler.DependencyMetadata{
		Ref: exact, Revision: int64Pointer(revision), ContentHash: hash,
	}
}

func unversionedMetadata(ref frozen.EnumeratedDependencyRef, hash string) compiler.DependencyMetadata {
	return compiler.DependencyMetadata{Ref: cloneRef(ref), ContentHash: hash}
}

func cloneRef(ref frozen.EnumeratedDependencyRef) frozen.EnumeratedDependencyRef {
	cloned := ref
	if ref.OwnerAgentVersion != nil {
		cloned.OwnerAgentVersion = int64Pointer(*ref.OwnerAgentVersion)
	}
	if ref.DependencyVersion != nil {
		cloned.DependencyVersion = int64Pointer(*ref.DependencyVersion)
	}
	return cloned
}

func cloneMetadata(value compiler.DependencyMetadata) compiler.DependencyMetadata {
	cloned := value
	cloned.Ref = cloneRef(value.Ref)
	if value.Revision != nil {
		cloned.Revision = int64Pointer(*value.Revision)
	}
	return cloned
}

func cloneResolved(value ResolvedDependency) ResolvedDependency {
	raw, err := json.Marshal(value)
	if err != nil {
		return ResolvedDependency{}
	}
	var cloned ResolvedDependency
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return ResolvedDependency{}
	}
	cloned.Metadata = cloneMetadata(cloned.Metadata)
	return cloned
}

func cloneAgentRecord(value registry.AgentRecord) (registry.AgentRecord, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return registry.AgentRecord{}, err
	}
	var cloned registry.AgentRecord
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return registry.AgentRecord{}, err
	}
	return cloned, nil
}

func cloneSkillRefs(values []registry.SkillRef) []registry.SkillRef {
	if len(values) == 0 {
		return nil
	}
	cloned := make([]registry.SkillRef, len(values))
	for index := range values {
		cloned[index] = values[index]
		if values[index].SkillVersion != nil {
			cloned[index].SkillVersion = cloneInt64(values[index].SkillVersion)
		}
	}
	return cloned
}

func cloneTeamWorkers(values []frozen.FrozenTeamWorker) []frozen.FrozenTeamWorker {
	raw, err := json.Marshal(values)
	if err != nil {
		return nil
	}
	var cloned []frozen.FrozenTeamWorker
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil
	}
	if cloned == nil {
		cloned = []frozen.FrozenTeamWorker{}
	}
	return cloned
}

func classifySourceError(err error, fallback, operation string) error {
	if nilInterface(err) {
		return newError(fallback, nil)
	}
	facts, complete := inspectErrorGraph(err)
	if !complete {
		return newError(fallback, nil)
	}
	if facts.infrastructure {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if facts.recognizedCode != "" {
		return newError(facts.recognizedCode, err)
	}
	if facts.hasCoded {
		return newError(fallback, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

type errorGraphFacts struct {
	recognizedCode string
	hasCoded       bool
	infrastructure bool
}

func inspectErrorGraph(err error) (errorGraphFacts, bool) {
	traversal := errorGraphTraversal{visited: make(map[error]struct{})}
	complete := traversal.collect(err, 0)
	return traversal.facts, complete
}

type errorGraphTraversal struct {
	facts   errorGraphFacts
	visited map[error]struct{}
	nodes   int
	edges   int
}

func (t *errorGraphTraversal) collect(current error, depth int) bool {
	if nilInterface(current) {
		return true
	}
	if depth > maxErrorGraphDepth || !reflect.TypeOf(current).Comparable() {
		return false
	}
	if _, seen := t.visited[current]; seen {
		return true
	}
	t.nodes++
	if t.nodes > maxErrorGraphNodes {
		return false
	}
	t.visited[current] = struct{}{}

	if directErrorMatch(current, context.Canceled) ||
		directErrorMatch(current, context.DeadlineExceeded) ||
		directErrorMatch(current, pgx.ErrTxClosed) {
		t.facts.infrastructure = true
	}
	if coded, ok := current.(interface{ Code() string }); ok && !nilInterface(coded) {
		code := coded.Code()
		if code != "" {
			t.facts.hasCoded = true
			if t.facts.recognizedCode == "" {
				if _, recognized := recognizedCodes[code]; recognized {
					t.facts.recognizedCode = code
				}
			}
		}
	}
	if many, ok := current.(interface{ Unwrap() []error }); ok {
		children := many.Unwrap()
		if len(children) > maxErrorGraphEdges-t.edges {
			return false
		}
		t.edges += len(children)
		for _, child := range children {
			if !t.collect(child, depth+1) {
				return false
			}
		}
		return true
	}
	if one, ok := current.(interface{ Unwrap() error }); ok {
		if t.edges == maxErrorGraphEdges {
			return false
		}
		t.edges++
		if !t.collect(one.Unwrap(), depth+1) {
			return false
		}
	}
	return true
}

func directErrorMatch(current, target error) bool {
	if nilInterface(current) || nilInterface(target) ||
		!reflect.TypeOf(current).Comparable() {
		return false
	}
	return current == target
}

func validateContext(ctx context.Context) error {
	if ctx == nil {
		return newError(CodeDependencyUnenumerable, nil)
	}
	return ctx.Err()
}

func newResolverGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}

func (r *Resolver) acquire(ctx context.Context) error {
	if err := validateContext(ctx); err != nil {
		return err
	}
	if r.gate == nil {
		return newError(CodeDependencyUnenumerable, nil)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.gate:
	}
	if err := validateContext(ctx); err != nil {
		r.release()
		return err
	}
	return nil
}

func (r *Resolver) release() {
	r.gate <- struct{}{}
}

func validVersion(value int64) bool {
	return value >= 1 && value <= frozen.MaxJCSSafeInteger
}

func int64Pointer(value int64) *int64 { return &value }
