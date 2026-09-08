package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/loom/stdlib"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func toolsTestRecord() registry.AgentRecord {
	return registry.AgentRecord{WorkspaceID: "ws", ID: "agent", Name: "worker", Version: 1, Role: "worker", Engine: "loom", GraphType: "standard", Spec: stdlib.AgentSpec{SystemPrompt: "Compute the requested result."}, MCPServers: []registry.MCPServerConfig{{ServerID: "mcp", Filter: []string{"calculate"}}}}
}

func toolsTestRegistry(t *testing.T) *DescriptorRegistry {
	t.Helper()
	r := NewDescriptorRegistry()
	for _, d := range []GraphFactoryDescriptor{NewStandardFrozenDescriptor(), NewStandardFrozenToolsDescriptor(), NewStandardFrozenCLIToolsDescriptor()} {
		if err := r.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestStandardFactorySelectionPreservesExplicitLegacyLoading(t *testing.T) {
	r := toolsTestRegistry(t)
	record := toolsTestRecord()
	key, err := r.SelectAgentFactoryKey(record)
	if err != nil || key != StandardFrozenToolsKey() {
		t.Fatalf("key=%#v err=%v", key, err)
	}
	if _, err := r.SelectFactoryKey("standard"); !errors.Is(err, ErrFactoryAmbiguous) {
		t.Fatalf("generic selector lost ambiguity check: %v", err)
	}
	legacy := NewStandardFrozenDescriptor().Key()
	if _, err := r.Lookup(legacy); err != nil {
		t.Fatal(err)
	}
	record.Engine = "claude"
	if got, err := r.SelectAgentFactoryKey(record); err != nil || got != StandardFrozenCLIToolsKey() {
		t.Fatalf("CLI was not bound: %v %v", got, err)
	}
	raw, err := (standardFrozenEnumerator{}).EncodeFactoryInput(t.Context(), toolsTestRecord(), nil)
	if err != nil || string(raw) != "{}" {
		t.Fatalf("v1 encoder changed: %s %v", raw, err)
	}
}

func TestStandardV2RoleProofKeepsExistingSnapshotValid(t *testing.T) {
	r := toolsTestRegistry(t)
	record := toolsTestRecord()
	legacy, err := r.describeWorkerRoleProofAtKey(t.Context(), record, nil, NewStandardFrozenDescriptor().Key())
	if err != nil {
		t.Fatal(err)
	}
	current, err := r.DescribeWorkerRoleProof(t.Context(), record, nil)
	if err != nil {
		t.Fatal(err)
	}
	if current == legacy {
		t.Fatal("MCP declaration missing from new proof")
	}
	for _, proof := range []FrozenWorkerRoleProof{legacy, current} {
		if err := r.VerifyWorkerRoleProof(t.Context(), record, proof); err != nil {
			t.Fatal(err)
		}
	}
	record.Name = "changed"
	if err := r.VerifyWorkerRoleProof(t.Context(), record, legacy); err == nil {
		t.Fatal("changed immutable record accepted legacy proof")
	}
}

type toolsMetadata struct {
	requests []frozen.EnumeratedDependencyRef
}

func (m *toolsMetadata) ResolveMetadata(_ context.Context, ref frozen.EnumeratedDependencyRef) (DependencyMetadata, error) {
	m.requests = append(m.requests, ref)
	version := int64(7)
	ref.DependencyVersion = &version
	return DependencyMetadata{Ref: ref, Revision: &version}, nil
}

func TestStandardV2EnumeratesAndValidatesExactMCPContract(t *testing.T) {
	record := toolsTestRecord()
	raw, err := (standardToolsEnumerator{}).EncodeFactoryInput(t.Context(), record, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := freezeDescriptorAgent(record, StandardFrozenToolsKey(), raw)
	if err != nil {
		t.Fatal(err)
	}
	metadata := &toolsMetadata{}
	manifest, err := (standardToolsEnumerator{}).EnumerateDependencies(t.Context(), agent, metadata)
	if err != nil || len(manifest.Dependencies) != 1 || manifest.Dependencies[0].DependencyType != "mcp_binding" || *manifest.Dependencies[0].DependencyVersion != 7 {
		t.Fatalf("manifest=%#v err=%v", manifest, err)
	}
	bundle := frozen.FrozenExecutionBundle{FactoryKey: StandardFrozenToolsKey(), Agent: agent, MCPBindings: []frozen.FrozenMCPBinding{{WorkspaceID: "ws", ServerID: "mcp", Transport: "http", Filter: []string{"calculate"}, Tools: []frozen.FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(`{"type":"object"}`)}}}}}
	if err := ValidateStandardMCPBindings(bundle); err != nil {
		t.Fatal(err)
	}
	bundle.MCPBindings[0].Tools[0].Name = "another"
	if err := ValidateStandardMCPBindings(bundle); err == nil {
		t.Fatal("missing configured tool accepted")
	}
	bundle.MCPBindings = nil
	if err := ValidateStandardMCPBindings(bundle); err == nil {
		t.Fatal("dropped MCP binding accepted")
	}
	record.MCPServers[0].Headers = map[string]string{"Authorization": "secret"}
	if _, err := (standardToolsEnumerator{}).EncodeFactoryInput(t.Context(), record, nil); err == nil {
		t.Fatal("inline credentials accepted")
	}
}

func TestStandardV3FreezesCLIWithoutLoomJournalContract(t *testing.T) {
	record := toolsTestRecord()
	record.Engine = "codex"
	record.RuntimeID = "runtime"
	enumerator := standardToolsEnumerator{cli: true}
	raw, err := enumerator.EncodeFactoryInput(t.Context(), record, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := frozen.DecodeStandardFactoryInputV2(raw); err == nil {
		t.Fatal("CLI input was admitted as Loom v2")
	}
	agent, err := freezeDescriptorAgent(record, StandardFrozenCLIToolsKey(), raw)
	if err != nil {
		t.Fatal(err)
	}
	metadata := &toolsMetadata{}
	manifest, err := enumerator.EnumerateDependencies(t.Context(), agent, metadata)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ref := range manifest.Dependencies {
		if ref.DependencyType == "mcp_binding" && ref.DependencyKey == "mcp" {
			found = true
		}
	}
	if !found {
		t.Fatal("CLI did not enumerate declared MCP binding")
	}
	binding := frozen.FrozenMCPBinding{WorkspaceID: "ws", ServerID: "mcp", Transport: "http", Filter: []string{"calculate"}, Tools: []frozen.FrozenToolDefinition{{Name: "calculate", InputSchema: json.RawMessage(`{"type":"object"}`)}}}
	bundle := frozen.FrozenExecutionBundle{FactoryKey: StandardFrozenCLIToolsKey(), Agent: agent, MCPBindings: []frozen.FrozenMCPBinding{binding}}
	if err := ValidateStandardMCPBindings(bundle); err != nil {
		t.Fatal(err)
	}
	bundle.MCPBindings = nil
	if err := ValidateStandardMCPBindings(bundle); err == nil {
		t.Fatal("CLI accepted dropped tool binding")
	}
	if _, err := (standardToolsEnumerator{}).EncodeFactoryInput(t.Context(), record, nil); err == nil {
		t.Fatal("CLI accepted Loom v2 contract")
	}
	record.Engine = "loom"
	if _, err := enumerator.EncodeFactoryInput(t.Context(), record, nil); err == nil {
		t.Fatal("Loom accepted CLI v3 contract")
	}
}
