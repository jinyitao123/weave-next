package teamforge

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/build/teambuild"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestFreezeDeclarativeWorkflowBindsDeliveryContract(t *testing.T) {
	spec := DeclarativeWorkflowSpecV1{
		SchemaVersion: 1, EntryNodeID: "deliver",
		InputContract: machine.OutputContract{Type: machine.ValueText}, OutputContract: machine.OutputContract{Type: machine.ValueText},
		Nodes: []DeclarativeWorkflowNodeV1{{ID: "deliver", Type: machine.NodeDeliver, Config: json.RawMessage(`{"result":{"source":"run_input","path":""}}`)}},
		Edges: []DeclarativeWorkflowEdgeV1{},
	}
	contract := &deliverable.DeliveryContract{
		Version: 1, Coverage: deliverable.CoverageExplicit, Output: deliverable.OutputRequirement{Type: "text"}, ExternalEffects: deliverable.ExternalEffectsNone,
		RequiredArtifacts: []deliverable.ArtifactRequirement{{ID: "report", Path: "outputs/report.md"}},
	}
	frozen, err := FreezeDeclarativeWorkflowSpecWithDeliveryContractV1(
		spec, nil,
		DeclarativeBuildBindingV1{BuildRunID: "build", BriefHash: strings.Repeat("a", 64), ContractHash: strings.Repeat("b", 64), BaselineHash: strings.Repeat("c", 64), AssetScope: teambuild.AssetScope{}},
		contract,
		func(machine.TriggerConfig, machine.GraphDefinition) (machine.Report, error) {
			return machine.Report{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	graph, report := machine.DecodeGraphDefinitionV1(frozen.GraphDefinition)
	if report != nil || graph.DeliveryContract == nil || graph.DeliveryContract.RequiredArtifacts[0].Path != "outputs/report.md" {
		t.Fatalf("frozen graph lost contract: report=%+v contract=%+v", report, graph.DeliveryContract)
	}
	if err := ValidateFrozenDeclarativeWorkflowSpecV1(frozen); err != nil {
		t.Fatalf("frozen contract did not survive revalidation: %v", err)
	}
}

func TestCompileDeclarativeHumanWait(t *testing.T) {
	spec := DeclarativeWorkflowSpecV1{
		SchemaVersion:  1,
		EntryNodeID:    "review",
		InputContract:  machine.OutputContract{Type: machine.ValueJSON},
		OutputContract: machine.OutputContract{Type: machine.ValueJSON},
		Nodes: []DeclarativeWorkflowNodeV1{
			{
				ID: "review", Type: machine.NodeWait,
				Config: json.RawMessage(`{
					"kind":"human",
					"resume_schema":{"type":"object","properties":{"decision":{"type":"string"}},"required":["decision"]},
					"task":{"title":"终审","instructions":"确认交付物","audience_ref":"editor"}
				}`),
			},
			{
				ID: "deliver", Type: machine.NodeDeliver,
				Config: json.RawMessage(`{"result":{"source":"node_output","node_id":"review"}}`),
			},
		},
		Edges: []DeclarativeWorkflowEdgeV1{{From: "review", To: "deliver", Route: machine.RouteSuccess}},
	}
	compiled, err := CompileDeclarativeWorkflowSpecV1(spec, nil)
	if err != nil {
		t.Fatalf("compile declarative human wait: %v", err)
	}
	wait := compiled.Graph.Nodes[0].Config.(machine.WaitConfig)
	if wait.Kind != machine.WaitKindHuman || wait.Task == nil || wait.Task.Title != "终审" {
		t.Fatalf("unexpected compiled wait: %+v", wait)
	}
}

func TestDeclarativeResultProtocolSurvivesCompilationAndEncoding(t *testing.T) {
	contract := machine.OutputContract{Type: machine.ValueJSON, Schema: machine.WorkbenchResultSchemaV1()}
	spec := DeclarativeWorkflowSpecV1{
		SchemaVersion: 1, EntryNodeID: "inspect", ResultProtocol: machine.ResultProtocolWorkbenchV1,
		InputContract: contract, OutputContract: contract,
		Nodes: []DeclarativeWorkflowNodeV1{
			{ID: "inspect", Type: machine.NodeLead, Output: &contract, Config: json.RawMessage(`{"instruction":"Inspect the supplied material."}`)},
			{ID: "deliver", Type: machine.NodeDeliver, Config: json.RawMessage(`{"result":{"source":"node_output","node_id":"inspect","path":""}}`)},
		},
		Edges: []DeclarativeWorkflowEdgeV1{{From: "inspect", To: "deliver", Route: machine.RouteSuccess}},
	}
	compiled, err := CompileDeclarativeWorkflowSpecV1(spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeWorkflowGraph(compiled.Graph)
	if err != nil {
		t.Fatal(err)
	}
	decoded, report := machine.DecodeGraphDefinitionV1(encoded)
	if report != nil && len(report.Issues) != 0 {
		t.Fatalf("decode encoded result protocol: %+v", report.Issues)
	}
	if decoded.ResultProtocol != machine.ResultProtocolWorkbenchV1 || !bytes.Equal(decoded.OutputContract.Schema, contract.Schema) {
		t.Fatalf("result protocol did not survive encoding: %+v", decoded)
	}
}

func TestDeclarativeWaitRejectsTimerKind(t *testing.T) {
	node := DeclarativeWorkflowNodeV1{
		ID: "timer", Type: machine.NodeWait,
		Config: json.RawMessage(`{"kind":"timer","resume_schema":{"type":"object"}}`),
	}
	_, _, err := compileDeclarativeNodeConfig(node, nil)
	if err == nil || !strings.Contains(err.Error(), "kind=human") {
		t.Fatalf("expected declarative timer rejection, got %v", err)
	}
}
