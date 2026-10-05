package machine

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
)

func TestBusinessCompletionDeclarationRequiresBoundFinalProtocol(t *testing.T) {
	const capability = "forge:action:record.submit"
	c := &deliverable.DeliveryContract{ExternalEffectsCheckID: "effects", RequiredChecks: []deliverable.CheckSpec{{ID: "effects", VerifierID: deliverycheck.BusinessReceiptsID, VerifierVersion: "v1", Parameters: json.RawMessage(`{"required_capability_ids":["forge:action:record.submit"],"when_authorized":true,"allow_needs_input":true}`)}}}
	output := OutputContract{Type: ValueJSON, Schema: WorkbenchResultSchemaV1()}
	graph := GraphDefinition{ResultProtocol: ResultProtocolWorkbenchV1, OutputContract: output, DeliveryContract: c, Nodes: []Node{{ID: "final", Type: NodeLead, Config: LeadConfig{}, Output: &output}, {ID: "deliver", Type: NodeDeliver, Config: DeliverConfig{Result: ValueRef{Source: ValueNodeOutput, NodeID: "final"}}}}}
	payload := frozen.ArtifactPayloadV1{Team: frozen.ArtifactTeamV1{LeadAgentID: "lead"}, Bundles: []frozen.FrozenExecutionBundle{{Agent: frozen.FrozenAgentRecord{AgentID: "lead", BusinessCapabilityIDs: []string{capability}}}}}
	if err := ValidateBusinessReceiptGraph(graph, payload); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"no protocol", "text output", "unbound capability", "non-agent source"} {
		t.Run(variant, func(t *testing.T) {
			g := graph
			p := payload
			switch variant {
			case "no protocol":
				g.ResultProtocol = ""
			case "text output":
				g.OutputContract = OutputContract{Type: ValueText}
			case "unbound capability":
				p.Bundles = nil
			case "non-agent source":
				g.Nodes = append([]Node(nil), graph.Nodes...)
				g.Nodes[0].Type = NodeTransform
			}
			if err := ValidateBusinessReceiptGraph(g, p); err == nil {
				t.Fatal("invalid completion declaration accepted")
			}
		})
	}
	if err := ValidateBusinessReceiptGraph(GraphDefinition{}, frozen.ArtifactPayloadV1{}); err != nil {
		t.Fatal("unconfigured workflow changed")
	}
}

func TestPublishingActionBoundGraphRequiresDeclaredCheck(t *testing.T) {
	const capability = "forge:action:record.submit"
	output := OutputContract{Type: ValueJSON, Schema: WorkbenchResultSchemaV1()}
	graph := GraphDefinition{ResultProtocol: ResultProtocolWorkbenchV1, OutputContract: output, Nodes: []Node{{ID: "final", Type: NodeLead, Config: LeadConfig{}, Output: &output}, {ID: "deliver", Type: NodeDeliver, Config: DeliverConfig{Result: ValueRef{Source: ValueNodeOutput, NodeID: "final"}}}}}
	bound := frozen.ArtifactPayloadV1{Team: frozen.ArtifactTeamV1{LeadAgentID: "lead"}, Bundles: []frozen.FrozenExecutionBundle{{Agent: frozen.FrozenAgentRecord{AgentID: "lead", BusinessCapabilityIDs: []string{capability}}}}}
	if err := RequireBusinessReceiptGraph(graph, bound); !errors.Is(err, ErrBusinessReceiptCheckRequired) {
		t.Fatalf("action-bound graph without check published: %v", err)
	}
	if err := ValidateBusinessReceiptGraph(graph, bound); err != nil {
		t.Fatalf("already-published artifact no longer loads: %v", err)
	}
	if DeclaresBusinessReceiptCheck(graph) {
		t.Fatal("undeclared graph reported a check")
	}
	unused := bound
	unused.Bundles = []frozen.FrozenExecutionBundle{{Agent: frozen.FrozenAgentRecord{AgentID: "unused", BusinessCapabilityIDs: []string{capability}}}}
	if err := RequireBusinessReceiptGraph(graph, unused); err != nil {
		t.Fatalf("actions of members outside the graph required a check: %v", err)
	}
	if err := RequireBusinessReceiptGraph(graph, frozen.ArtifactPayloadV1{Team: frozen.ArtifactTeamV1{LeadAgentID: "lead"}, Bundles: []frozen.FrozenExecutionBundle{{Agent: frozen.FrozenAgentRecord{AgentID: "lead"}}}}); err != nil {
		t.Fatalf("read-only graph required a check: %v", err)
	}
	graph.DeliveryContract = &deliverable.DeliveryContract{ExternalEffectsCheckID: "effects", RequiredChecks: []deliverable.CheckSpec{{ID: "effects", VerifierID: deliverycheck.BusinessReceiptsID, VerifierVersion: "v1", Parameters: json.RawMessage(`{"required_capability_ids":["forge:action:record.submit"],"when_authorized":true,"allow_needs_input":true}`)}}}
	if err := RequireBusinessReceiptGraph(graph, bound); err != nil || !DeclaresBusinessReceiptCheck(graph) {
		t.Fatalf("declared graph rejected: %v", err)
	}
}
