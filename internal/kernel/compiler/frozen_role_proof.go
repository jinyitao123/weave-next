package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

// FrozenWorkerRoleProof is the descriptor-derived identity proof persisted by
// team admission. It contains no live resolver or mutable registry reference.
type FrozenWorkerRoleProof struct {
	Role                  string
	AgentContentHash      string
	CapabilitySchema      int
	CapabilityContentHash string
}

// DescribeWorkerRoleProof freezes one exact worker record through the selected
// factory descriptor and hashes the same DTOs consumed by frozen compilation.
func (r *DescriptorRegistry) DescribeWorkerRoleProof(
	ctx context.Context,
	record registry.AgentRecord,
	encoder CredentialRefEncoder,
) (FrozenWorkerRoleProof, error) {
	if r == nil || record.Role != "worker" || record.ID == "" || record.Version < 1 {
		return FrozenWorkerRoleProof{}, ErrFrozenCapabilityMismatch
	}
	graphType := record.GraphType
	if graphType == "" {
		graphType = record.Spec.GraphType
	}
	key, err := r.SelectAgentFactoryKey(record)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	if graphType == "" {
		graphType = "standard"
	}
	if graphType != key.FactoryID {
		return FrozenWorkerRoleProof{}, ErrFactoryUnknown
	}
	return r.describeWorkerRoleProofAtKey(ctx, record, encoder, key)
}

func (r *DescriptorRegistry) describeWorkerRoleProofAtKey(ctx context.Context, record registry.AgentRecord, encoder CredentialRefEncoder, key frozen.FactoryKey) (FrozenWorkerRoleProof, error) {
	descriptor, err := r.Lookup(key)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	if descriptor.DescribeCapability == nil {
		return FrozenWorkerRoleProof{}, ErrFactoryDescriptorInvalid
	}
	if encoder == nil {
		encoder = rejectingCredentialRefEncoder{}
	}
	factoryInput, err := descriptor.EnumerateDependencies.EncodeFactoryInput(ctx, record, encoder)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	agent, err := freezeDescriptorAgent(record, key, factoryInput)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	capability, err := descriptor.DescribeCapability(ctx, agent)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	capability, err = bindCapabilityIdentity(agent, capability)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	agentHash, err := frozen.HashDTO(agent, frozen.PreorderFrozenAgentRecord)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	capabilityHash, err := frozen.HashDTO(capability, frozen.PreorderCapabilityManifest)
	if err != nil {
		return FrozenWorkerRoleProof{}, err
	}
	return FrozenWorkerRoleProof{
		Role: capability.Role, AgentContentHash: agentHash,
		CapabilitySchema:      capability.SchemaVersion,
		CapabilityContentHash: capabilityHash,
	}, nil
}

// VerifyWorkerRoleProof accepts an existing v1 admission proof only when the
// exact immutable record still reproduces it. New admissions use the selected
// factory, while active snapshots do not change identity after an upgrade.
func (r *DescriptorRegistry) VerifyWorkerRoleProof(ctx context.Context, record registry.AgentRecord, expected FrozenWorkerRoleProof) error {
	current, err := r.DescribeWorkerRoleProof(ctx, record, nil)
	if err == nil && current == expected {
		return nil
	}
	key, keyErr := r.SelectAgentFactoryKey(record)
	if keyErr == nil && key == StandardFrozenToolsKey() {
		legacyKey := NewStandardFrozenDescriptor().Key()
		legacy, legacyErr := r.describeWorkerRoleProofAtKey(ctx, record, nil, legacyKey)
		if legacyErr == nil && legacy == expected {
			return nil
		}
	}
	if err != nil {
		return err
	}
	return ErrFrozenCapabilityMismatch
}

type rejectingCredentialRefEncoder struct{}

func (rejectingCredentialRefEncoder) EncodeReference(
	context.Context,
	string,
	string,
	string,
) (frozen.CredentialReference, error) {
	return frozen.CredentialReference{}, fmt.Errorf("credential references are unavailable during team admission")
}

func bindCapabilityIdentity(
	agent frozen.FrozenAgentRecord,
	capability frozen.CapabilityManifest,
) (frozen.CapabilityManifest, error) {
	if agent.Role == "" {
		return frozen.CapabilityManifest{}, ErrFrozenCapabilityMismatch
	}
	hash, err := frozen.HashDTO(agent, frozen.PreorderFrozenAgentRecord)
	if err != nil {
		return frozen.CapabilityManifest{}, err
	}
	capability.SchemaVersion = frozen.CapabilityManifestSchemaVersion
	capability.Role = agent.Role
	capability.AgentContentHash = hash
	return capability, nil
}

func freezeDescriptorAgent(
	record registry.AgentRecord,
	key frozen.FactoryKey,
	input json.RawMessage,
) (frozen.FrozenAgentRecord, error) {
	profiles := make(map[string]frozen.FrozenAgentProfile, len(record.Spec.Profiles))
	for name, profile := range record.Spec.Profiles {
		profiles[name] = frozen.FrozenAgentProfile{
			SystemAddition: profile.SystemAddition, Greeting: profile.Greeting,
		}
	}
	value := frozen.FrozenAgentRecord{
		SchemaVersion: frozen.FrozenSchemaVersion,
		WorkspaceID:   record.WorkspaceID, AgentID: record.ID,
		AgentVersion: int64(record.Version), Name: record.Name,
		DisplayName: record.DisplayName, Role: record.Role, Engine: record.Engine,
		RuntimeID: record.RuntimeID, Model: record.Model,
		SystemPrompt: record.Spec.SystemPrompt,
		Identity: frozen.FrozenAgentIdentity{
			Core: record.Spec.Identity.Core, Extended: record.Spec.Identity.Extended,
			Raw: record.Spec.Identity.Raw,
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
			MaxCostUSD: record.MaxCostUSD, MaxTokens: record.MaxTokens,
			MaxOutputTokens: int64(record.MaxOutputTokens), StepBudget: record.StepBudget,
			ToolLoopControl: record.ToolLoopControl,
			MaxToolRepeats:  int64(record.MaxToolRepeats),
		},
		Fallback: frozen.FrozenFallback{
			Models:  append([]string(nil), record.FallbackModels...),
			Retries: int64(record.FallbackRetries),
		},
		GraphType: key.FactoryID, FactoryInput: append(json.RawMessage(nil), input...),
	}
	if record.OutputSchema != nil {
		value.OutputSchema = append(json.RawMessage(nil), (*record.OutputSchema)...)
	}
	if record.MemoryConfig != nil {
		value.MemoryConfig = &frozen.FrozenMemoryConfig{
			Enabled: record.MemoryConfig.Enabled, TopK: int64(record.MemoryConfig.TopK),
			AutoRemember: record.MemoryConfig.AutoRemember, Scope: record.MemoryConfig.Scope,
		}
	}
	for index, slot := range record.MemorySlots {
		value.MemorySlots[index] = frozen.FrozenMemorySlot{
			Key: slot.Key, Label: slot.Label, Description: slot.Description,
		}
	}
	if record.Guard != nil {
		value.Guard = &frozen.FrozenGuard{
			Enabled: record.Guard.Enabled, MaxInputLen: int64(record.Guard.MaxInputLen),
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
		return frozen.FrozenAgentRecord{}, ErrDependencyUnenumerable
	}
	if normalized.GraphType != key.FactoryID {
		return frozen.FrozenAgentRecord{}, errors.New("agent graph type does not match factory identity")
	}
	return normalized, nil
}
