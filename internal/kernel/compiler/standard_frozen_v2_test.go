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

func TestBusinessOnlyLoomPublicationSelectsDurableFactory(t *testing.T) {
	r := toolsTestRegistry(t)
	plain := toolsTestRecord()
	plain.MCPServers = nil
	if key, err := r.SelectAgentFactoryKey(plain); err != nil || key != NewStandardFrozenDescriptor().Key() {
		t.Fatalf("plain member changed its legacy contract: %v %v", key, err)
	}
	for _, bindingsOnly := range []bool{false, true} {
		record := toolsTestRecord()
		record.MCPServers = nil
		if bindingsOnly {
			record.BusinessCapabilityBindings = []frozen.BusinessCapabilityBinding{{CapabilityID: "forge:action:record.Submit"}}
		} else {
			record.BusinessCapabilityIDs = []string{"forge:action:record.Submit"}
		}
		key, err := r.SelectAgentFactoryKey(record)
		if err != nil || key != StandardFrozenToolsKey() {
			t.Fatalf("business-only member lacks durable factory: key=%+v err=%v", key, err)
		}
		if record.ToolLoopControl != nil {
			t.Fatal("durability must not require an unrelated budget configuration")
		}
		for _, engine := range []string{"codex", "claude"} {
			record.Engine = engine
			if _, err := r.SelectAgentFactoryKey(record); err == nil {
				t.Fatalf("business execution without durable operation support admitted: %s", engine)
			}
		}
		record.Engine, record.GraphType = "loom", "declarative"
		if _, err := r.SelectAgentFactoryKey(record); err == nil {
			t.Fatal("unproven custom business execution admitted")
		}
	}
}

func TestBusinessOnlyExistingRoleProofStillRequiresItsExactRecord(t *testing.T) {
	r := toolsTestRegistry(t)
	record := toolsTestRecord()
	record.MCPServers = nil
	record.BusinessCapabilityIDs = []string{"forge:action:record.Submit"}
	legacy, err := r.describeWorkerRoleProofAtKey(t.Context(), record, nil, NewStandardFrozenDescriptor().Key())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.VerifyWorkerRoleProof(t.Context(), record, legacy); err != nil {
		t.Fatal(err)
	}
	record.BusinessCapabilityIDs = []string{"forge:action:record.Delete"}
	if err := r.VerifyWorkerRoleProof(t.Context(), record, legacy); err == nil {
		t.Fatal("changed business authority reused the old role proof")
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

func TestPublishedAgentFreezesBusinessCapabilities(t *testing.T) {
	record := toolsTestRecord()
	record.BusinessCapabilityIDs = []string{"forge:action:sales_contract.ContractSubmit", "forge:action:sales_contract.ContractRead"}
	raw, err := (standardToolsEnumerator{}).EncodeFactoryInput(t.Context(), record, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := freezeDescriptorAgent(record, StandardFrozenToolsKey(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.BusinessCapabilityIDs) != 2 || agent.BusinessCapabilityIDs[0] != "forge:action:sales_contract.ContractRead" || agent.BusinessCapabilityIDs[1] != "forge:action:sales_contract.ContractSubmit" {
		t.Fatalf("business capabilities=%v", agent.BusinessCapabilityIDs)
	}
	record.BusinessCapabilityIDs[0] = "forge:action:sales_contract.ContractDelete"
	if agent.BusinessCapabilityIDs[1] != "forge:action:sales_contract.ContractSubmit" {
		t.Fatal("frozen business capabilities retained mutable registry storage")
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

func TestControlledMemberPublicationPinsBudgetAndRejectsUnsupportedTopology(t *testing.T) {
	descriptors := toolsTestRegistry(t)
	record := toolsTestRecord()
	record.MCPServers = nil
	record.ToolLoopControl = &frozen.ToolLoopControl{SliceRounds: 2, InitialTotalRounds: 5}
	key, err := descriptors.SelectAgentFactoryKey(record)
	if err != nil || key != StandardFrozenToolsKey() {
		t.Fatalf("key=%+v err=%v", key, err)
	}
	descriptor, err := descriptors.Lookup(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := descriptor.EnumerateDependencies.EncodeFactoryInput(t.Context(), record, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := freezeDescriptorAgent(record, key, raw)
	if err != nil {
		t.Fatal(err)
	}
	capability, err := descriptor.DescribeCapability(t.Context(), agent)
	if err != nil || !capability.MayYield || len(capability.InteractiveStepIDs) != 1 {
		t.Fatalf("capability=%+v err=%v", capability, err)
	}
	proof, err := descriptors.DescribeWorkerRoleProof(t.Context(), record, nil)
	if err != nil {
		t.Fatal(err)
	}
	record.ToolLoopControl = &frozen.ToolLoopControl{SliceRounds: 2, InitialTotalRounds: 6}
	if err := descriptors.VerifyWorkerRoleProof(t.Context(), record, proof); err == nil {
		t.Fatal("budget change retained published role proof")
	}
	for _, change := range []func(*registry.AgentRecord){
		func(r *registry.AgentRecord) { r.Engine = "codex" },
		func(r *registry.AgentRecord) { r.GraphType = "declarative" },
		func(r *registry.AgentRecord) { r.GraphType = ""; r.Spec.GraphType = "custom" },
		func(r *registry.AgentRecord) { r.SubAgents = []registry.SubAgentRef{{Name: "child"}} },
		func(r *registry.AgentRecord) { r.Permissions.Ask = []string{"write"} },
		func(r *registry.AgentRecord) { r.ToolLoopControl = &frozen.ToolLoopControl{InitialTotalRounds: 3} },
	} {
		invalid := record
		change(&invalid)
		if _, err := descriptors.SelectAgentFactoryKey(invalid); err == nil {
			t.Fatal("unsupported controlled topology selected")
		}
		if _, err := descriptor.EnumerateDependencies.EncodeFactoryInput(t.Context(), invalid, nil); err == nil {
			t.Fatal("unsupported controlled topology frozen")
		}
	}
}
