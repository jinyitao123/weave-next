package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

type factoryDependencyKey struct {
	SchemaVersion  int    `json:"schema_version"`
	FactoryID      string `json:"factory_id"`
	FactoryVersion string `json:"factory_version"`
	CompilerABI    string `json:"compiler_abi"`
}

type credentialDependencyKey struct {
	Kind       frozen.CredentialKind `json:"kind"`
	ResourceID string                `json:"resource_id"`
	Slot       string                `json:"slot"`
}

type derivedDependencyIdentity struct {
	ownerType, ownerID string
	ownerAgentVersion  int64
	dependencyType     string
	dependencyKey      string
}

// BuildCandidateDependencies builds the deterministic impact-analysis index
// for one publication candidate.
func BuildCandidateDependencies(
	workspaceID, workflowID string,
	workflowVersion int,
	manifest frozen.FrozenDependencyManifest,
	bundles []frozen.FrozenExecutionBundle,
	deliveryTargets []frozen.FrozenDeliveryTarget,
) ([]TeamWorkflowDependency, error) {
	dependencies := make([]TeamWorkflowDependency, 0, len(manifest.Dependencies))
	for _, dependency := range manifest.Dependencies {
		dependencies = append(dependencies, TeamWorkflowDependency{
			WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: workflowVersion,
			OwnerType: dependency.OwnerType, OwnerID: dependency.OwnerID,
			OwnerAgentVersion: cloneDependencyVersion(dependency.OwnerAgentVersion),
			DependencyType:    dependency.DependencyType, DependencyKey: dependency.DependencyKey,
			DependencyVersion: cloneDependencyVersion(dependency.DependencyVersion),
			ContentHash:       dependency.ContentHash,
		})
	}

	seen := make(map[derivedDependencyIdentity]struct{})
	appendDerived := func(dependency TeamWorkflowDependency) {
		identity := derivedDependencyIdentity{
			ownerType: dependency.OwnerType, ownerID: dependency.OwnerID,
			ownerAgentVersion: dependencyIdentityVersion(dependency.OwnerAgentVersion),
			dependencyType:    dependency.DependencyType, dependencyKey: dependency.DependencyKey,
		}
		if _, exists := seen[identity]; exists {
			return
		}
		seen[identity] = struct{}{}
		dependencies = append(dependencies, dependency)
	}
	derived := func(ownerType, ownerID string, ownerVersion *int64, dependencyType, key, hash string) TeamWorkflowDependency {
		return TeamWorkflowDependency{
			WorkspaceID: workspaceID, WorkflowID: workflowID, WorkflowVersion: workflowVersion,
			OwnerType: ownerType, OwnerID: ownerID, OwnerAgentVersion: cloneDependencyVersion(ownerVersion),
			DependencyType: dependencyType, DependencyKey: key, ContentHash: hash,
		}
	}

	for _, bundle := range bundles {
		ownerVersion := bundle.Agent.AgentVersion
		key, hash, err := buildFactoryDependency(bundle.FactoryKey)
		if err != nil {
			return nil, err
		}
		appendDerived(derived("agent", bundle.Agent.AgentID, &ownerVersion, "factory", key, hash))
		for _, credential := range bundle.Credentials {
			key, hash, err := buildCredentialDependency(credential)
			if err != nil {
				return nil, err
			}
			appendDerived(derived("agent", bundle.Agent.AgentID, &ownerVersion, "credential_reference", key, hash))
		}
	}
	for _, target := range deliveryTargets {
		credentials := make([]frozen.CredentialReference, 0, 1+len(target.CredentialBindings))
		credentials = append(credentials, target.AccessRef)
		for _, binding := range target.CredentialBindings {
			credentials = append(credentials, binding.CredentialRef)
		}
		for _, credential := range credentials {
			key, hash, err := buildCredentialDependency(credential)
			if err != nil {
				return nil, err
			}
			appendDerived(derived("workflow", workflowID, nil, "credential_reference", key, hash))
		}
	}

	sort.Slice(dependencies, func(i, j int) bool {
		return frozen.CompareFrozenDependency(dependencyAsFrozenRef(dependencies[i]), dependencyAsFrozenRef(dependencies[j])) < 0
	})
	return dependencies, nil
}

func buildFactoryDependency(key frozen.FactoryKey) (string, string, error) {
	return canonicalDependencyKey(factoryDependencyKey{
		SchemaVersion: frozen.FrozenSchemaVersion,
		FactoryID:     key.FactoryID, FactoryVersion: key.FactoryVersion, CompilerABI: key.CompilerABI,
	})
}

func buildCredentialDependency(reference frozen.CredentialReference) (string, string, error) {
	key, _, err := canonicalDependencyKey(credentialDependencyKey{
		Kind: reference.Kind, ResourceID: reference.ResourceID, Slot: reference.Slot,
	})
	if err != nil {
		return "", "", err
	}
	hash, err := frozen.HashDTO(reference, frozen.PreorderCredentialReference)
	return key, hash, err
}

func canonicalDependencyKey(value any) (string, string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	canonical, err := frozen.CanonicalizeJSON(raw)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(canonical)
	return string(canonical), hex.EncodeToString(sum[:]), nil
}

func dependencyAsFrozenRef(dependency TeamWorkflowDependency) frozen.FrozenDependencyRef {
	return frozen.FrozenDependencyRef{
		EnumeratedDependencyRef: frozen.EnumeratedDependencyRef{
			WorkspaceID: dependency.WorkspaceID, OwnerType: dependency.OwnerType, OwnerID: dependency.OwnerID,
			OwnerAgentVersion: dependency.OwnerAgentVersion, DependencyType: dependency.DependencyType,
			DependencyKey: dependency.DependencyKey, DependencyVersion: dependency.DependencyVersion,
		},
		ContentHash: dependency.ContentHash,
	}
}

func dependencyIdentityVersion(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func cloneDependencyVersion(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}
