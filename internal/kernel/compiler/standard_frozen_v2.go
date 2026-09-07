package compiler

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

const StandardFrozenToolsVersion = "2"

func StandardFrozenToolsKey() frozen.FactoryKey {
	return frozen.FactoryKey{FactoryID: standardFrozenFactoryID, FactoryVersion: StandardFrozenToolsVersion, CompilerABI: standardFrozenCompilerABI}
}

// NewStandardFrozenToolsDescriptor adds declared, frozen MCP contracts while
// preserving the v1 descriptor for already-published artifacts.
func NewStandardFrozenToolsDescriptor() GraphFactoryDescriptor {
	descriptor := NewStandardFrozenDescriptor()
	descriptor.FactoryVersion = StandardFrozenToolsVersion
	descriptor.EnumerateDependencies = standardToolsEnumerator{}
	descriptor.Compile = func(ctx context.Context, bundle frozen.FrozenExecutionBundle, _ FrozenResolver, opts FrozenBuildOpts) (*loom.Graph, frozen.CapabilityManifest, error) {
		return compileStandardFrozenVersion(ctx, bundle, opts, StandardFrozenToolsVersion)
	}
	return descriptor
}

type standardToolsEnumerator struct{}

func (standardToolsEnumerator) FreezeSchema() FreezeSchema {
	return FreezeSchema{SchemaID: "weave-standard-factory-input/2", SchemaVersion: 2}
}

func (standardToolsEnumerator) EncodeFactoryInput(ctx context.Context, record registry.AgentRecord, encoder CredentialRefEncoder) (json.RawMessage, error) {
	if _, err := (standardFrozenEnumerator{}).EncodeFactoryInput(ctx, record, encoder); err != nil {
		return nil, err
	}
	if record.Engine != "" && record.Engine != "loom" {
		return nil, normalizeCompilerError(CodeDependencyUnenumerable, fmt.Errorf("standard v2 requires a Loom member"))
	}
	input := frozen.StandardFactoryInputV2{SchemaVersion: 2, MCPServers: []frozen.StandardMCPDeclaration{}}
	for _, server := range record.MCPServers {
		if server.ServerID == "" || server.URL != "" || len(server.Headers) != 0 {
			return nil, normalizeCompilerError(CodeDependencyUnenumerable, fmt.Errorf("member %q requires a managed MCP server reference", record.Name))
		}
		input.MCPServers = append(input.MCPServers, frozen.StandardMCPDeclaration{
			ServerID: server.ServerID, Filter: slices.Clone(server.Filter), WriteTools: slices.Clone(server.WriteTools),
		})
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	normalized, err := frozen.DecodeStandardFactoryInputV2(raw)
	if err != nil {
		return nil, normalizeCompilerError(CodeDependencyUnenumerable, err)
	}
	return json.Marshal(normalized)
}

func (standardToolsEnumerator) EnumerateDependencies(ctx context.Context, agent frozen.FrozenAgentRecord, metadata MetadataResolver) (frozen.EnumeratedDependencyManifest, error) {
	input, err := frozen.DecodeStandardFactoryInputV2(agent.FactoryInput)
	if err != nil || agent.Engine != "loom" {
		return frozen.EnumeratedDependencyManifest{}, normalizeCompilerError(CodeDependencyUnenumerable, err)
	}
	common := agent
	common.FactoryInput = json.RawMessage(`{}`)
	manifest, err := (standardFrozenEnumerator{}).EnumerateDependencies(ctx, common, metadata)
	if err != nil {
		return frozen.EnumeratedDependencyManifest{}, err
	}
	for _, server := range input.MCPServers {
		ownerVersion := agent.AgentVersion
		resolved, err := metadata.ResolveMetadata(ctx, frozen.EnumeratedDependencyRef{
			WorkspaceID: agent.WorkspaceID, OwnerType: "agent", OwnerID: agent.AgentID, OwnerAgentVersion: &ownerVersion,
			DependencyType: "mcp_binding", DependencyKey: server.ServerID,
		})
		if err != nil {
			return frozen.EnumeratedDependencyManifest{}, err
		}
		if resolved.Ref.DependencyVersion == nil {
			return frozen.EnumeratedDependencyManifest{}, ErrFrozenManifestMismatch
		}
		manifest.Dependencies = append(manifest.Dependencies, resolved.Ref)
	}
	return manifest, nil
}

// SelectAgentFactoryKey is the explicit publication/admission policy. Generic
// SelectFactoryKey retains its ambiguity check and old artifacts use Lookup.
func (r *DescriptorRegistry) SelectAgentFactoryKey(record registry.AgentRecord) (frozen.FactoryKey, error) {
	graphType := record.GraphType
	if graphType == "" {
		graphType = record.Spec.GraphType
	}
	if graphType != "" && graphType != "standard" {
		return r.SelectFactoryKey(graphType)
	}
	if (record.Engine == "" || record.Engine == "loom") && len(record.MCPServers) > 0 {
		key := StandardFrozenToolsKey()
		if _, err := r.Lookup(key); err != nil {
			return frozen.FactoryKey{}, err
		}
		return key, nil
	}
	key := frozen.FactoryKey{FactoryID: standardFrozenFactoryID, FactoryVersion: standardFrozenFactoryVersion, CompilerABI: standardFrozenCompilerABI}
	if _, err := r.Lookup(key); err != nil {
		return frozen.FactoryKey{}, err
	}
	return key, nil
}

// ValidateStandardMCPBindings checks the frozen declaration, actual resolved
// bindings and tool catalog together, before compilation or host construction.
func ValidateStandardMCPBindings(bundle frozen.FrozenExecutionBundle) error {
	if bundle.FactoryKey != StandardFrozenToolsKey() {
		return nil
	}
	input, err := frozen.DecodeStandardFactoryInputV2(bundle.Agent.FactoryInput)
	if err != nil {
		return normalizeCompilerError(CodeFactoryCompileFailed, err)
	}
	if bundle.Agent.Engine != "loom" || len(input.MCPServers) != len(bundle.MCPBindings) {
		return standardFrozenCompileError("standard v2 MCP bindings do not match the member declaration")
	}
	bindings := make(map[string]frozen.FrozenMCPBinding, len(bundle.MCPBindings))
	for _, binding := range bundle.MCPBindings {
		if _, found := bindings[binding.ServerID]; found {
			return standardFrozenCompileError("standard v2 MCP binding is repeated")
		}
		bindings[binding.ServerID] = binding
	}
	owners := map[string]string{}
	for _, server := range input.MCPServers {
		binding, ok := bindings[server.ServerID]
		if !ok || binding.WorkspaceID != bundle.Agent.WorkspaceID || binding.Transport != "http" || len(binding.Tools) == 0 {
			return standardFrozenCompileError(fmt.Sprintf("member %q MCP server %q lacks a supported, probed tool contract", bundle.Agent.Name, server.ServerID))
		}
		if !slices.Equal(server.Filter, binding.Filter) || !slices.Equal(server.WriteTools, binding.WriteTools) {
			return standardFrozenCompileError(fmt.Sprintf("member %q MCP server %q policy differs from its declaration", bundle.Agent.Name, server.ServerID))
		}
		if _, err := frozen.NormalizeToolDefinitions(binding.Tools); err != nil {
			return standardFrozenCompileError(fmt.Sprintf("member %q MCP server %q tool contract is invalid", bundle.Agent.Name, server.ServerID))
		}
		for _, tool := range binding.Tools {
			if len(server.Filter) > 0 && !slices.Contains(server.Filter, tool.Name) {
				return standardFrozenCompileError(fmt.Sprintf("member %q tool %q is outside MCP server %q filter", bundle.Agent.Name, tool.Name, server.ServerID))
			}
			if previous, exists := owners[tool.Name]; exists {
				return standardFrozenCompileError(fmt.Sprintf("member %q tool %q conflicts between MCP servers %q and %q", bundle.Agent.Name, tool.Name, previous, server.ServerID))
			}
			owners[tool.Name] = server.ServerID
		}
		for _, name := range server.Filter {
			if owners[name] != server.ServerID {
				return standardFrozenCompileError(fmt.Sprintf("member %q required tool %q is missing from MCP server %q", bundle.Agent.Name, name, server.ServerID))
			}
		}
	}
	return nil
}
