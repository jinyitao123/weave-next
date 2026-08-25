package compiler

import (
	"context"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// SkillRefSource exposes an agent's explicit skill bindings to frozen
// dependency enumerators without widening the MetadataResolver contract.
// Freeze-time resolvers (freezer.Resolver) return the exact AgentRecord
// bindings; compile-time resolvers (manifestMetadataResolver) derive
// registry_version bindings from the stored frozen manifest so re-enumeration
// never touches a live store. A resolver that does not implement the
// interface enumerates no skill dependencies, preserving pre-D3b behavior for
// legacy records and tests.
type SkillRefSource interface {
	SkillRefs(ctx context.Context, workspaceID, agentID string, agentVersion int64) ([]registry.SkillRef, error)
}

// EnumerateFrozenSkillRefs appends exact registry_version skill dependencies
// for the agent in record order. Each registry_version binding produces one
// DependencyType=skill ref with DependencyKey "registry:<skill_id>" and the
// exact pinned DependencyVersion. legacy/builtin bindings are live-only and
// are intentionally not frozen here; the publish-time validator rejects them
// before this point (ValidateFrozenSkillRefs), so they can never be silently
// dropped or guessed as latest.
func EnumerateFrozenSkillRefs(
	ctx context.Context,
	agent frozen.FrozenAgentRecord,
	metadata MetadataResolver,
) ([]frozen.EnumeratedDependencyRef, error) {
	source, ok := metadata.(SkillRefSource)
	if !ok {
		return nil, nil
	}
	refs, err := source.SkillRefs(ctx, agent.WorkspaceID, agent.AgentID, agent.AgentVersion)
	if err != nil {
		return nil, err
	}
	dependencies := make([]frozen.EnumeratedDependencyRef, 0, len(refs))
	ownerVersion := agent.AgentVersion
	for index := range refs {
		ref := &refs[index]
		if ref.SourceType != registry.SourceTypeRegistryVersion {
			continue
		}
		if ref.SkillID == "" || ref.SkillVersion == nil || *ref.SkillVersion < 1 {
			return nil, fmt.Errorf(
				"%w: agent %q registry_version skill ref %d must pin an exact positive skill_version",
				registry.ErrSkillVersionRequired, agent.Name, index,
			)
		}
		resolved, err := metadata.ResolveMetadata(ctx, frozen.EnumeratedDependencyRef{
			WorkspaceID: agent.WorkspaceID, OwnerType: "agent", OwnerID: agent.AgentID,
			OwnerAgentVersion: &ownerVersion, DependencyType: "skill",
			DependencyKey: "registry:" + ref.SkillID, DependencyVersion: ref.SkillVersion,
		})
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, resolved.Ref)
	}
	return dependencies, nil
}

// ValidateFrozenSkillRefs is the publish-time binding validator (spec §6.3).
// registry_version refs must pin an exact positive version (fail-closed for
// records that bypass the create/update contract, e.g. direct store writes);
// name-only legacy/builtin refs cannot be frozen and are rejected with the
// stable workflow_skill_version_required sentinel instead of being silently
// ignored or guessed as latest. Legacy Spec.Skills entries are unchanged:
// they remain live-only and keep the pre-D3b publish behavior.
func ValidateFrozenSkillRefs(record registry.AgentRecord) error {
	for index := range record.SkillRefs {
		ref := &record.SkillRefs[index]
		switch ref.SourceType {
		case registry.SourceTypeRegistryVersion:
			if ref.SkillID == "" || ref.SkillVersion == nil || *ref.SkillVersion < 1 {
				return fmt.Errorf(
					"%w: agent %q registry_version skill ref %d must pin an exact positive skill_version",
					registry.ErrSkillVersionRequired, record.Name, index,
				)
			}
		case registry.SourceTypeLegacy, registry.SourceTypeBuiltin:
			return fmt.Errorf(
				"%w: agent %q name-only skill binding %q must pin an exact registry skill_version before publication",
				registry.ErrSkillVersionRequired, record.Name, ref.Name,
			)
		default:
			return fmt.Errorf("agent %q skill ref %d: unknown source_type %q", record.Name, index, ref.SourceType)
		}
	}
	return nil
}
