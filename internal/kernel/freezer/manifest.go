package freezer

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

const CodeFrozenDependencyUndeclared = "workflow_frozen_dependency_undeclared"

// AgentFreezeRequest contains the inputs required to freeze one exact agent
// dependency before resolving the complete manifest.
type AgentFreezeRequest struct {
	Ref          frozen.EnumeratedDependencyRef
	Usage        AgentUsage
	Key          frozen.FactoryKey
	FactoryInput json.RawMessage
}

// ResolveDependenciesEagerTx freezes requested agents in order, then eagerly
// resolves the complete manifest using the caller-owned transaction.
func ResolveDependenciesEagerTx(
	ctx context.Context,
	source *Resolver,
	agents []AgentFreezeRequest,
	enumerated frozen.EnumeratedDependencyManifest,
) (frozen.FrozenDependencyManifest, compiler.FrozenResolver, error) {
	for _, agent := range agents {
		if _, err := source.ResolveAgentVersion(
			ctx, agent.Ref, agent.Usage, agent.Key, agent.FactoryInput,
		); err != nil {
			return frozen.FrozenDependencyManifest{}, nil, err
		}
	}
	return ResolveManifest(ctx, enumerated, source)
}

// ArtifactDependencySet is the complete secretless DTO set carried by a
// publication artifact.
type ArtifactDependencySet struct {
	Agents          []frozen.FrozenAgentRecord
	Skills          []frozen.FrozenSkill
	MCPBindings     []frozen.FrozenMCPBinding
	ModelBindings   []frozen.FrozenModelBinding
	RuntimeBindings []frozen.FrozenRuntimeBinding
	DeliveryTargets []frozen.FrozenDeliveryTarget
}

type frozenDependencyKey struct {
	dependencyCacheKey
	contentHash string
}

type manifestAgentKey struct {
	agentID string
	version int64
}

type artifactPayloadKey struct {
	dependencyType, workspaceID, dependencyKey string
	dependencyVersionSet                       bool
	dependencyVersion                          int64
}

type manifestResolver struct {
	agents          map[manifestAgentKey]ResolvedDependency
	skills          map[frozenDependencyKey]ResolvedDependency
	mcpBindings     map[frozenDependencyKey]ResolvedDependency
	modelBindings   map[frozenDependencyKey]ResolvedDependency
	runtimeBindings map[frozenDependencyKey]ResolvedDependency
	deliveryTargets map[frozenDependencyKey]ResolvedDependency
}

var _ compiler.FrozenResolver = (*manifestResolver)(nil)

// ResolveManifest resolves every enumerated dependency eagerly and returns a
// resolver that is independent of the source transaction after construction.
func ResolveManifest(
	ctx context.Context,
	enumerated frozen.EnumeratedDependencyManifest,
	source *Resolver,
) (frozen.FrozenDependencyManifest, compiler.FrozenResolver, error) {
	if err := validateContext(ctx); err != nil {
		return frozen.FrozenDependencyManifest{}, nil, err
	}
	if source == nil || enumerated.SchemaVersion != frozen.FrozenSchemaVersion ||
		enumerated.Dependencies == nil {
		return frozen.FrozenDependencyManifest{}, nil, newError(CodeDependencyUnenumerable, nil)
	}

	refs := make([]frozen.FrozenDependencyRef, 0, len(enumerated.Dependencies))
	set := ArtifactDependencySet{}
	payloads := make(map[artifactPayloadKey]resolvedIdentity)
	for _, ref := range enumerated.Dependencies {
		if err := validateManifestEnumeratedRef(ctx, source, ref); err != nil {
			return frozen.FrozenDependencyManifest{}, nil, err
		}
		metadata, err := source.ResolveMetadata(ctx, ref)
		if err != nil {
			return frozen.FrozenDependencyManifest{}, nil, err
		}
		if !equalEnumeratedRef(metadata.Ref, ref) ||
			!metadataRevisionMatches(ref, metadata.Revision) ||
			!validManifestHash(metadata.ContentHash) {
			return frozen.FrozenDependencyManifest{}, nil, newError(CodeFrozenManifestMismatch, nil)
		}
		resolved, ok := source.Lookup(ref)
		if !ok || !reflect.DeepEqual(resolved.Metadata, metadata) {
			return frozen.FrozenDependencyManifest{}, nil, newError(CodeFrozenManifestMismatch, nil)
		}
		identity, err := validateResolvedDependency(ref, resolved)
		if err != nil {
			return frozen.FrozenDependencyManifest{}, nil, err
		}
		if identity.contentHash != metadata.ContentHash {
			return frozen.FrozenDependencyManifest{}, nil, newError(CodeFrozenManifestMismatch, nil)
		}
		payloadKey := artifactKey(identity)
		if previous, exists := payloads[payloadKey]; exists {
			if previous.contentHash != identity.contentHash {
				return frozen.FrozenDependencyManifest{}, nil, newError(CodeFrozenManifestMismatch, nil)
			}
		} else {
			payloads[payloadKey] = identity
			appendResolvedDependency(&set, resolved)
		}
		refs = append(refs, frozen.FrozenDependencyRef{
			EnumeratedDependencyRef: cloneRef(ref),
			ContentHash:             metadata.ContentHash,
		})
	}

	_, manifest, err := frozen.BuildFrozenDependencyManifest(refs)
	if err != nil {
		return frozen.FrozenDependencyManifest{}, nil, newError(CodeFrozenManifestMismatch, err)
	}
	if err := frozen.ValidateManifest(manifest); err != nil {
		return frozen.FrozenDependencyManifest{}, nil, newError(CodeFrozenManifestMismatch, err)
	}
	resolver, err := NewArtifactResolver(manifest, set)
	if err != nil {
		return frozen.FrozenDependencyManifest{}, nil, err
	}
	return manifest, resolver, nil
}

// NewArtifactResolver reconstructs a frozen resolver solely from artifact
// bytes. It performs all validation eagerly and retains no mutable source.
func NewArtifactResolver(
	manifest frozen.FrozenDependencyManifest,
	set ArtifactDependencySet,
) (compiler.FrozenResolver, error) {
	if err := frozen.ValidateManifest(manifest); err != nil {
		return nil, newError(CodeFrozenManifestMismatch, err)
	}
	for _, ref := range manifest.Dependencies {
		if err := validateFrozenManifestRef(ref); err != nil {
			return nil, err
		}
	}

	resolver := &manifestResolver{
		agents:          make(map[manifestAgentKey]ResolvedDependency),
		skills:          make(map[frozenDependencyKey]ResolvedDependency),
		mcpBindings:     make(map[frozenDependencyKey]ResolvedDependency),
		modelBindings:   make(map[frozenDependencyKey]ResolvedDependency),
		runtimeBindings: make(map[frozenDependencyKey]ResolvedDependency),
		deliveryTargets: make(map[frozenDependencyKey]ResolvedDependency),
	}
	consumed := make([]bool, len(manifest.Dependencies))
	payloads := make(map[artifactPayloadKey]struct{})
	add := func(resolved ResolvedDependency) error {
		identity, err := validateResolvedDependency(
			resolved.Metadata.Ref,
			resolved,
		)
		if err != nil {
			return err
		}
		payloadKey := artifactKey(identity)
		if _, exists := payloads[payloadKey]; exists {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		payloads[payloadKey] = struct{}{}

		matched := make([]int, 0, 1)
		for index, ref := range manifest.Dependencies {
			if ref.DependencyType != identity.dependencyType ||
				ref.WorkspaceID != identity.workspaceID ||
				ref.DependencyKey != identity.dependencyKey ||
				!optionalInt64Equal(ref.DependencyVersion, identity.dependencyVersion) {
				continue
			}
			if ref.ContentHash != identity.contentHash || consumed[index] {
				return newError(CodeFrozenManifestMismatch, nil)
			}
			matched = append(matched, index)
		}
		if len(matched) == 0 {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		for _, index := range matched {
			consumed[index] = true
			exactRef := manifest.Dependencies[index]
			exact := resolved
			exact.Metadata = compiler.DependencyMetadata{
				Ref:         cloneRef(exactRef.EnumeratedDependencyRef),
				ContentHash: exactRef.ContentHash,
			}
			if exactRef.DependencyVersion != nil {
				exact.Metadata.Revision = manifestInt64Pointer(*exactRef.DependencyVersion)
			}
			if err := resolver.store(exactRef, exact); err != nil {
				return err
			}
		}
		return nil
	}

	for index := range set.Agents {
		value := set.Agents[index]
		ref := identityRef("agent", value.WorkspaceID, value.AgentID, value.AgentVersion)
		hash, err := frozen.HashDTO(value, frozen.PreorderFrozenAgentRecord)
		if err != nil {
			return nil, newError(CodeFrozenManifestMismatch, err)
		}
		resolved := ResolvedDependency{
			Metadata: metadataFor(ref, value.AgentVersion, hash),
			Agent:    &value,
		}
		if err := add(resolved); err != nil {
			return nil, err
		}
	}
	for index := range set.Skills {
		value := set.Skills[index]
		dependencyKey, version, err := skillIdentity(value)
		if err != nil {
			return nil, err
		}
		hash, err := frozen.HashDTO(value, frozen.PreorderFrozenSkill)
		if err != nil || value.ContentHash != hash {
			return nil, newError(CodeFrozenManifestMismatch, err)
		}
		ref := identityRefOptional("skill", value.WorkspaceID, dependencyKey, version)
		resolved := ResolvedDependency{
			Metadata: metadataForOptional(ref, version, hash),
			Skill:    &value,
		}
		if err := add(resolved); err != nil {
			return nil, err
		}
	}
	for index := range set.MCPBindings {
		value := set.MCPBindings[index]
		hash, err := frozen.HashDTO(value, frozen.PreorderFrozenMCPBinding)
		if err != nil || value.ContentHash != hash {
			return nil, newError(CodeFrozenManifestMismatch, err)
		}
		ref := identityRef("mcp_binding", value.WorkspaceID, value.ServerID, value.ServerRevision)
		resolved := ResolvedDependency{Metadata: metadataFor(ref, value.ServerRevision, hash), MCPBinding: &value}
		if err := add(resolved); err != nil {
			return nil, err
		}
	}
	for index := range set.ModelBindings {
		value := set.ModelBindings[index]
		hash, err := frozen.HashDTO(value, frozen.PreorderFrozenModelBinding)
		if err != nil || value.ContentHash != hash {
			return nil, newError(CodeFrozenManifestMismatch, err)
		}
		ref := identityRef("model_binding", value.WorkspaceID, value.ModelID, value.ProviderRevision)
		resolved := ResolvedDependency{Metadata: metadataFor(ref, value.ProviderRevision, hash), ModelBinding: &value}
		if err := add(resolved); err != nil {
			return nil, err
		}
	}
	for index := range set.RuntimeBindings {
		value := set.RuntimeBindings[index]
		hash, err := frozen.HashDTO(value, frozen.PreorderFrozenRuntimeBinding)
		if err != nil || value.ContentHash != hash {
			return nil, newError(CodeFrozenManifestMismatch, err)
		}
		ref := identityRef("runtime_binding", value.WorkspaceID, value.RuntimeID, value.RuntimeRevision)
		resolved := ResolvedDependency{Metadata: metadataFor(ref, value.RuntimeRevision, hash), RuntimeBinding: &value}
		if err := add(resolved); err != nil {
			return nil, err
		}
	}
	for index := range set.DeliveryTargets {
		value := set.DeliveryTargets[index]
		hash, err := frozen.HashDTO(value, frozen.PreorderFrozenDeliveryTarget)
		if err != nil || value.ContentHash != hash {
			return nil, newError(CodeFrozenManifestMismatch, err)
		}
		ref := identityRef("delivery_target", value.WorkspaceID, value.TargetID, value.TargetRevision)
		resolved := ResolvedDependency{Metadata: metadataFor(ref, value.TargetRevision, hash), DeliveryTarget: &value}
		if err := add(resolved); err != nil {
			return nil, err
		}
	}
	for _, used := range consumed {
		if !used {
			return nil, newError(CodeFrozenManifestMismatch, nil)
		}
	}
	return resolver, nil
}

type resolvedIdentity struct {
	dependencyType    string
	workspaceID       string
	dependencyKey     string
	dependencyVersion *int64
	contentHash       string
}

func artifactKey(identity resolvedIdentity) artifactPayloadKey {
	key := artifactPayloadKey{
		dependencyType: identity.dependencyType,
		workspaceID:    identity.workspaceID,
		dependencyKey:  identity.dependencyKey,
	}
	if identity.dependencyVersion != nil {
		key.dependencyVersionSet = true
		key.dependencyVersion = *identity.dependencyVersion
	}
	return key
}

func validateResolvedDependency(
	ref frozen.EnumeratedDependencyRef,
	resolved ResolvedDependency,
) (resolvedIdentity, error) {
	if !equalEnumeratedRef(ref, resolved.Metadata.Ref) {
		return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
	}
	payloads := 0
	if resolved.Agent != nil {
		payloads++
	}
	if resolved.Skill != nil {
		payloads++
	}
	if resolved.MCPBinding != nil {
		payloads++
	}
	if resolved.ModelBinding != nil {
		payloads++
	}
	if resolved.RuntimeBinding != nil {
		payloads++
	}
	if resolved.DeliveryTarget != nil {
		payloads++
	}
	if payloads != 1 {
		return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
	}

	identity := resolvedIdentity{
		dependencyType: ref.DependencyType, workspaceID: ref.WorkspaceID,
		dependencyKey: ref.DependencyKey, dependencyVersion: cloneInt64(ref.DependencyVersion),
		contentHash: resolved.Metadata.ContentHash,
	}
	var (
		hash string
		err  error
	)
	switch ref.DependencyType {
	case "agent":
		if resolved.Agent == nil ||
			resolved.Agent.WorkspaceID != ref.WorkspaceID ||
			resolved.Agent.AgentID != ref.DependencyKey ||
			ref.DependencyVersion == nil ||
			resolved.Agent.AgentVersion != *ref.DependencyVersion {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
		hash, err = frozen.HashDTO(*resolved.Agent, frozen.PreorderFrozenAgentRecord)
	case "skill":
		if resolved.Skill == nil || resolved.Skill.WorkspaceID != ref.WorkspaceID {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
		key, version, identityErr := skillIdentity(*resolved.Skill)
		if identityErr != nil || key != ref.DependencyKey ||
			!optionalInt64Equal(version, ref.DependencyVersion) {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, identityErr)
		}
		hash, err = frozen.HashDTO(*resolved.Skill, frozen.PreorderFrozenSkill)
		if err == nil && resolved.Skill.ContentHash != hash {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
	case "mcp_binding":
		if resolved.MCPBinding == nil ||
			resolved.MCPBinding.WorkspaceID != ref.WorkspaceID ||
			resolved.MCPBinding.ServerID != ref.DependencyKey ||
			ref.DependencyVersion == nil ||
			resolved.MCPBinding.ServerRevision != *ref.DependencyVersion {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
		hash, err = frozen.HashDTO(*resolved.MCPBinding, frozen.PreorderFrozenMCPBinding)
		if err == nil && resolved.MCPBinding.ContentHash != hash {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
	case "model_binding":
		if resolved.ModelBinding == nil ||
			resolved.ModelBinding.WorkspaceID != ref.WorkspaceID ||
			resolved.ModelBinding.ModelID != ref.DependencyKey ||
			ref.DependencyVersion == nil ||
			resolved.ModelBinding.ProviderRevision != *ref.DependencyVersion {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
		hash, err = frozen.HashDTO(*resolved.ModelBinding, frozen.PreorderFrozenModelBinding)
		if err == nil && resolved.ModelBinding.ContentHash != hash {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
	case "runtime_binding":
		if resolved.RuntimeBinding == nil ||
			resolved.RuntimeBinding.WorkspaceID != ref.WorkspaceID ||
			resolved.RuntimeBinding.RuntimeID != ref.DependencyKey ||
			ref.DependencyVersion == nil ||
			resolved.RuntimeBinding.RuntimeRevision != *ref.DependencyVersion {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
		hash, err = frozen.HashDTO(*resolved.RuntimeBinding, frozen.PreorderFrozenRuntimeBinding)
		if err == nil && resolved.RuntimeBinding.ContentHash != hash {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
	case "delivery_target":
		if resolved.DeliveryTarget == nil ||
			resolved.DeliveryTarget.WorkspaceID != ref.WorkspaceID ||
			resolved.DeliveryTarget.TargetID != ref.DependencyKey ||
			ref.DependencyVersion == nil ||
			resolved.DeliveryTarget.TargetRevision != *ref.DependencyVersion {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
		hash, err = frozen.HashDTO(*resolved.DeliveryTarget, frozen.PreorderFrozenDeliveryTarget)
		if err == nil && resolved.DeliveryTarget.ContentHash != hash {
			return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, nil)
		}
	default:
		return resolvedIdentity{}, newError(CodeDependencyUnenumerable, nil)
	}
	if err != nil || hash != resolved.Metadata.ContentHash || !validManifestHash(hash) {
		return resolvedIdentity{}, newError(CodeFrozenManifestMismatch, err)
	}
	return identity, nil
}

func appendResolvedDependency(set *ArtifactDependencySet, resolved ResolvedDependency) {
	switch {
	case resolved.Agent != nil:
		set.Agents = append(set.Agents, *resolved.Agent)
	case resolved.Skill != nil:
		set.Skills = append(set.Skills, *resolved.Skill)
	case resolved.MCPBinding != nil:
		set.MCPBindings = append(set.MCPBindings, *resolved.MCPBinding)
	case resolved.ModelBinding != nil:
		set.ModelBindings = append(set.ModelBindings, *resolved.ModelBinding)
	case resolved.RuntimeBinding != nil:
		set.RuntimeBindings = append(set.RuntimeBindings, *resolved.RuntimeBinding)
	case resolved.DeliveryTarget != nil:
		set.DeliveryTargets = append(set.DeliveryTargets, *resolved.DeliveryTarget)
	}
}

func validateManifestEnumeratedRef(
	ctx context.Context,
	source *Resolver,
	ref frozen.EnumeratedDependencyRef,
) error {
	if err := source.validateRef(ctx, ref); err != nil {
		return err
	}
	if (ref.DependencyType == "model_binding" ||
		ref.DependencyType == "runtime_binding") &&
		ref.DependencyVersion == nil {
		return newError(CodeDependencyVersionRequired, nil)
	}
	return nil
}

func validateFrozenManifestRef(ref frozen.FrozenDependencyRef) error {
	switch ref.DependencyType {
	case "agent":
		if ref.OwnerType != "agent" ||
			ref.OwnerID != ref.DependencyKey ||
			ref.OwnerAgentVersion == nil ||
			ref.DependencyVersion == nil ||
			!validVersion(*ref.OwnerAgentVersion) ||
			!validVersion(*ref.DependencyVersion) ||
			*ref.OwnerAgentVersion != *ref.DependencyVersion {
			return newError(CodeDependencyUnenumerable, nil)
		}
	case "mcp_binding", "model_binding", "runtime_binding", "delivery_target":
		if ref.DependencyVersion == nil || !validVersion(*ref.DependencyVersion) {
			return newError(CodeDependencyVersionRequired, nil)
		}
	case "skill":
		prefix, name, ok := strings.Cut(ref.DependencyKey, ":")
		if !ok || name == "" {
			return newError(CodeDependencyUnenumerable, nil)
		}
		switch prefix {
		case "registry":
			if ref.DependencyVersion == nil || !validVersion(*ref.DependencyVersion) {
				return newError(CodeDependencyVersionRequired, nil)
			}
		case "builtin", "inline":
			if ref.DependencyVersion != nil {
				return newError(CodeDependencyUnenumerable, nil)
			}
		default:
			return newError(CodeDependencyUnenumerable, nil)
		}
	case "credential_reference", "factory":
		return newError(CodeDependencyUnenumerable, nil)
	default:
		return newError(CodeDependencyUnenumerable, nil)
	}
	return nil
}

func (r *manifestResolver) store(ref frozen.FrozenDependencyRef, resolved ResolvedDependency) error {
	cloned := cloneResolved(resolved)
	key := frozenDependencyKey{dependencyCacheKey: cacheKey(ref.EnumeratedDependencyRef), contentHash: ref.ContentHash}
	switch ref.DependencyType {
	case "agent":
		if cloned.Agent == nil || ref.DependencyVersion == nil {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		agentKey := manifestAgentKey{agentID: ref.DependencyKey, version: *ref.DependencyVersion}
		if _, exists := r.agents[agentKey]; exists {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		r.agents[agentKey] = cloned
	case "skill":
		if _, exists := r.skills[key]; exists {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		r.skills[key] = cloned
	case "mcp_binding":
		if _, exists := r.mcpBindings[key]; exists {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		r.mcpBindings[key] = cloned
	case "model_binding":
		if _, exists := r.modelBindings[key]; exists {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		r.modelBindings[key] = cloned
	case "runtime_binding":
		if _, exists := r.runtimeBindings[key]; exists {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		r.runtimeBindings[key] = cloned
	case "delivery_target":
		if _, exists := r.deliveryTargets[key]; exists {
			return newError(CodeFrozenManifestMismatch, nil)
		}
		r.deliveryTargets[key] = cloned
	default:
		return newError(CodeDependencyUnenumerable, nil)
	}
	return nil
}

func (r *manifestResolver) Agent(
	ctx context.Context,
	agentID string,
	version int64,
) (frozen.FrozenAgentRecord, error) {
	if err := validateManifestAccessorContext(ctx); err != nil {
		return frozen.FrozenAgentRecord{}, err
	}
	if r == nil || agentID == "" || agentID != strings.TrimSpace(agentID) || !validVersion(version) {
		return frozen.FrozenAgentRecord{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	resolved, ok := r.agents[manifestAgentKey{agentID: agentID, version: version}]
	if !ok {
		return frozen.FrozenAgentRecord{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	cloned := cloneResolved(resolved)
	if cloned.Agent == nil {
		return frozen.FrozenAgentRecord{}, newError(CodeFrozenManifestMismatch, nil)
	}
	return *cloned.Agent, nil
}

func (r *manifestResolver) Skill(
	ctx context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenSkill, error) {
	if r == nil {
		return frozen.FrozenSkill{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	resolved, err := r.lookup(ctx, ref, "skill", r.skills)
	if err != nil {
		return frozen.FrozenSkill{}, err
	}
	if resolved.Skill == nil {
		return frozen.FrozenSkill{}, newError(CodeFrozenManifestMismatch, nil)
	}
	return *resolved.Skill, nil
}

func (r *manifestResolver) MCPBinding(
	ctx context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenMCPBinding, error) {
	if r == nil {
		return frozen.FrozenMCPBinding{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	resolved, err := r.lookup(ctx, ref, "mcp_binding", r.mcpBindings)
	if err != nil {
		return frozen.FrozenMCPBinding{}, err
	}
	if resolved.MCPBinding == nil {
		return frozen.FrozenMCPBinding{}, newError(CodeFrozenManifestMismatch, nil)
	}
	return *resolved.MCPBinding, nil
}

func (r *manifestResolver) ModelBinding(
	ctx context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenModelBinding, error) {
	if r == nil {
		return frozen.FrozenModelBinding{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	resolved, err := r.lookup(ctx, ref, "model_binding", r.modelBindings)
	if err != nil {
		return frozen.FrozenModelBinding{}, err
	}
	if resolved.ModelBinding == nil {
		return frozen.FrozenModelBinding{}, newError(CodeFrozenManifestMismatch, nil)
	}
	return *resolved.ModelBinding, nil
}

func (r *manifestResolver) RuntimeBinding(
	ctx context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenRuntimeBinding, error) {
	if r == nil {
		return frozen.FrozenRuntimeBinding{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	resolved, err := r.lookup(ctx, ref, "runtime_binding", r.runtimeBindings)
	if err != nil {
		return frozen.FrozenRuntimeBinding{}, err
	}
	if resolved.RuntimeBinding == nil {
		return frozen.FrozenRuntimeBinding{}, newError(CodeFrozenManifestMismatch, nil)
	}
	return *resolved.RuntimeBinding, nil
}

func (r *manifestResolver) DeliveryTarget(
	ctx context.Context,
	ref frozen.FrozenDependencyRef,
) (frozen.FrozenDeliveryTarget, error) {
	if r == nil {
		return frozen.FrozenDeliveryTarget{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	resolved, err := r.lookup(ctx, ref, "delivery_target", r.deliveryTargets)
	if err != nil {
		return frozen.FrozenDeliveryTarget{}, err
	}
	if resolved.DeliveryTarget == nil {
		return frozen.FrozenDeliveryTarget{}, newError(CodeFrozenManifestMismatch, nil)
	}
	return *resolved.DeliveryTarget, nil
}

func (r *manifestResolver) lookup(
	ctx context.Context,
	ref frozen.FrozenDependencyRef,
	dependencyType string,
	values map[frozenDependencyKey]ResolvedDependency,
) (ResolvedDependency, error) {
	if err := validateManifestAccessorContext(ctx); err != nil {
		return ResolvedDependency{}, err
	}
	if r == nil || ref.DependencyType != dependencyType ||
		!validManifestHash(ref.ContentHash) {
		return ResolvedDependency{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	value, ok := values[frozenDependencyKey{
		dependencyCacheKey: cacheKey(ref.EnumeratedDependencyRef),
		contentHash:        ref.ContentHash,
	}]
	if !ok {
		return ResolvedDependency{}, newError(CodeFrozenDependencyUndeclared, nil)
	}
	return cloneResolved(value), nil
}

func validateManifestAccessorContext(ctx context.Context) error {
	if ctx == nil {
		return newError(CodeFrozenDependencyUndeclared, nil)
	}
	return ctx.Err()
}

func skillIdentity(value frozen.FrozenSkill) (string, *int64, error) {
	switch value.SourceType {
	case "builtin", "inline":
		if value.Name == "" || value.SkillID != "" || value.SkillVersion != nil {
			return "", nil, newError(CodeFrozenManifestMismatch, nil)
		}
		return value.SourceType + ":" + value.Name, nil, nil
	case "registry_version":
		if value.SkillID == "" || value.SkillVersion == nil || !validVersion(*value.SkillVersion) {
			return "", nil, newError(CodeFrozenManifestMismatch, nil)
		}
		return "registry:" + value.SkillID, cloneInt64(value.SkillVersion), nil
	default:
		return "", nil, newError(CodeFrozenManifestMismatch, nil)
	}
}

func identityRef(dependencyType, workspaceID, key string, version int64) frozen.EnumeratedDependencyRef {
	return identityRefOptional(dependencyType, workspaceID, key, manifestInt64Pointer(version))
}

func identityRefOptional(
	dependencyType, workspaceID, key string,
	version *int64,
) frozen.EnumeratedDependencyRef {
	return frozen.EnumeratedDependencyRef{
		WorkspaceID: workspaceID, DependencyType: dependencyType,
		DependencyKey: key, DependencyVersion: cloneInt64(version),
	}
}

func metadataForOptional(
	ref frozen.EnumeratedDependencyRef,
	version *int64,
	hash string,
) compiler.DependencyMetadata {
	metadata := compiler.DependencyMetadata{Ref: cloneRef(ref), ContentHash: hash}
	if version != nil {
		metadata.Revision = cloneInt64(version)
	}
	return metadata
}

func equalEnumeratedRef(left, right frozen.EnumeratedDependencyRef) bool {
	return left.WorkspaceID == right.WorkspaceID &&
		left.OwnerType == right.OwnerType &&
		left.OwnerID == right.OwnerID &&
		optionalInt64Equal(left.OwnerAgentVersion, right.OwnerAgentVersion) &&
		left.DependencyType == right.DependencyType &&
		left.DependencyKey == right.DependencyKey &&
		optionalInt64Equal(left.DependencyVersion, right.DependencyVersion)
}

func metadataRevisionMatches(ref frozen.EnumeratedDependencyRef, revision *int64) bool {
	return optionalInt64Equal(ref.DependencyVersion, revision)
}

func optionalInt64Equal(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return manifestInt64Pointer(*value)
}

func manifestInt64Pointer(value int64) *int64 { return &value }

func validManifestHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			if char < 'a' || char > 'f' {
				return false
			}
		}
	}
	return true
}
