package freezer

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestFreezeAgentPreservesBusinessCapabilities(t *testing.T) {
	record := registry.AgentRecord{
		WorkspaceID: "workspace",
		ID:          "agent",
		Version:     2,
		Name:        "reviewer",
		Role:        "worker",
		Engine:      "loom",
		Model:       "model",
		GraphType:   "standard",
		BusinessCapabilityIDs: []string{
			"forge:action:sales_contract.ContractSubmit",
			"forge:action:sales_contract.ContractRead",
		},
		BusinessCapabilityBindings: []frozen.BusinessCapabilityBinding{{
			CapabilityID: "forge:action:sales_contract.ContractSubmit",
			Parameters: []frozen.BusinessCapabilityParameterBinding{
				{Name: "material_file_id", Source: frozen.BusinessSourceMaterialID},
				{Name: "material_file_ids", Source: frozen.BusinessSourceMaterialIDs},
			},
		}},
	}

	agent, err := freezeAgent(record, frozen.FactoryKey{FactoryID: "standard"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(agent.BusinessCapabilityIDs) != 2 ||
		agent.BusinessCapabilityIDs[0] != "forge:action:sales_contract.ContractRead" ||
		agent.BusinessCapabilityIDs[1] != "forge:action:sales_contract.ContractSubmit" {
		t.Fatalf("business capabilities=%v", agent.BusinessCapabilityIDs)
	}
	if len(agent.BusinessCapabilityBindings) != 1 || len(agent.BusinessCapabilityBindings[0].Parameters) != 2 ||
		agent.BusinessCapabilityBindings[0].Parameters[0].Name != "material_file_id" ||
		agent.BusinessCapabilityBindings[0].Parameters[1].Source != frozen.BusinessSourceMaterialIDs {
		t.Fatalf("business capability bindings=%+v", agent.BusinessCapabilityBindings)
	}
	record.BusinessCapabilityIDs[0] = "forge:action:sales_contract.ContractDelete"
	if agent.BusinessCapabilityIDs[1] != "forge:action:sales_contract.ContractSubmit" {
		t.Fatal("frozen business capabilities retained mutable registry storage")
	}
	record.BusinessCapabilityBindings[0].Parameters[0].Source = frozen.BusinessSourceMaterialsManifest
	if agent.BusinessCapabilityBindings[0].Parameters[0].Source != frozen.BusinessSourceMaterialID {
		t.Fatal("frozen business parameter bindings retained mutable registry storage")
	}
	record.BusinessCapabilityBindings[0].Parameters[1].Source = frozen.BusinessSourceMaterialsManifest
	if agent.BusinessCapabilityBindings[0].Parameters[1].Source != frozen.BusinessSourceMaterialIDs {
		t.Fatal("frozen multi-file parameter binding retained mutable registry storage")
	}
}
