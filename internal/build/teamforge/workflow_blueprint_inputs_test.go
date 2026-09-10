package teamforge

import (
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestCompileWorkflowBlueprintCarriesDeliveryContract(t *testing.T) {
	blueprint := WorkflowBlueprint{
		Template: WorkflowBlueprintResearchSummary, LeadInstruction: "Coordinate",
		ParallelWorkers: []WorkflowBlueprintWorker{
			{AgentID: "researcher", AgentVersion: 1, ResultRequirement: "Research"},
			{AgentID: "reviewer", AgentVersion: 1, ResultRequirement: "Review"},
		},
		Finalizer: &WorkflowBlueprintWorker{AgentID: "editor", AgentVersion: 1, ResultRequirement: "Deliver"},
		DeliveryContract: &deliverable.DeliveryContract{
			Version: 1, Coverage: deliverable.CoverageExplicit,
			Output: deliverable.OutputRequirement{Type: "text"}, ExternalEffects: deliverable.ExternalEffectsNone,
			RequiredArtifacts: []deliverable.ArtifactRequirement{{ID: "report", Path: "outputs/report.md"}},
		},
	}
	compiled, problems := CompileWorkflowBlueprint(blueprint)
	if len(problems) != 0 || compiled.Graph.DeliveryContract == nil || compiled.Graph.DeliveryContract.RequiredArtifacts[0].Path != "outputs/report.md" {
		t.Fatalf("compiled graph lost contract: problems=%+v contract=%+v", problems, compiled.Graph.DeliveryContract)
	}
	compiled.Graph.DeliveryContract.RequiredArtifacts[0].Path = "changed"
	if blueprint.DeliveryContract.RequiredArtifacts[0].Path != "outputs/report.md" {
		t.Fatal("compiled graph aliases mutable blueprint contract")
	}
}

func TestSynthesisBlueprintPreservesOriginalTaskAfterFreeze(t *testing.T) {
	for _, template := range []WorkflowBlueprintTemplate{WorkflowBlueprintResearchSummary, WorkflowBlueprintParallelReview} {
		t.Run(string(template), func(t *testing.T) {
			blueprint := WorkflowBlueprint{
				Template: template, LeadInstruction: "Coordinate a Workbench task using the supplied facts.",
				ParallelWorkers: []WorkflowBlueprintWorker{
					{AgentID: "facts", AgentVersion: 1, ResultRequirement: "Extract the supplied facts."},
					{AgentID: "gaps", AgentVersion: 1, ResultRequirement: "List information missing from the supplied facts."},
				},
				Finalizer: &WorkflowBlueprintWorker{AgentID: "writer", AgentVersion: 1, ResultRequirement: "Return a brief containing the original facts."},
			}
			compiled, problems := CompileWorkflowBlueprint(blueprint)
			if len(problems) != 0 {
				t.Fatalf("compile: %#v", problems)
			}
			raw, err := encodeWorkflowGraph(compiled.Graph)
			if err != nil {
				t.Fatal(err)
			}
			graph, report := machine.DecodeGraphDefinitionV1(raw)
			if report != nil && len(report.Issues) != 0 {
				t.Fatalf("decode published graph: %#v", report)
			}
			agentNodes := 0
			for _, node := range graph.Nodes {
				if node.Type != machine.NodeLead && node.Type != machine.NodeWorker {
					continue
				}
				agentNodes++
				// A lead response that omits facts or claims orchestration is blocked
				// must never be the only material available to downstream workers.
				input, ok := node.Inputs["run_input"]
				if !ok || input.ExpectedType != machine.ValueText || input.Value.Source != machine.ValueRunInput ||
					input.Value.Path != "" || input.Value.NodeID != "" {
					t.Fatalf("%s cannot read the unchanged original task: %#v", node.ID, node.Inputs)
				}
				instruction := ""
				switch config := node.Config.(type) {
				case machine.LeadConfig:
					instruction = config.Instruction
				case machine.WorkerConfig:
					instruction = config.ResultRequirement
					upstream := node.Inputs["brief"].Value
					if node.ID == "finalizer" {
						upstream = node.Inputs["results"].Value
					}
					if upstream.Source != machine.ValueNodeOutput || upstream.NodeID == "" {
						t.Fatalf("%s lost upstream analysis: %#v", node.ID, node.Inputs)
					}
				}
				if !strings.Contains(instruction, synthesisNodeProtocol) {
					t.Fatalf("%s lacks the platform/node responsibility boundary", node.ID)
				}
			}
			if agentNodes != 4 || blueprint.Finalizer.ResultRequirement != "Return a brief containing the original facts." {
				t.Fatal("compilation changed the agreed team or blueprint input")
			}
		})
	}
}
