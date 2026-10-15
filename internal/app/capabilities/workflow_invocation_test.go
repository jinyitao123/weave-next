package capabilities

import (
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestWorkflowResultProducerUsesExactDeliverReference(t *testing.T) {
	payload := frozen.ArtifactPayloadV1{
		Team: frozen.ArtifactTeamV1{LeadAgentID: "unused-lead", LeadAgentVersion: 1},
		Bundles: []frozen.FrozenExecutionBundle{
			{Agent: frozen.FrozenAgentRecord{AgentID: "unused-lead", AgentVersion: 1, Model: "unused-model", Engine: "loom"}},
			{Agent: frozen.FrozenAgentRecord{AgentID: "player", AgentVersion: 2, Model: "player-model", Engine: "loom"}},
		},
	}
	graph := machine.GraphDefinition{Nodes: []machine.Node{
		{ID: "captain", Type: machine.NodeLead, Config: machine.LeadConfig{}},
		{ID: "player", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "player", AgentVersion: 2}},
		{ID: "deliver", Type: machine.NodeDeliver, Config: machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "player"}}},
	}}
	agentID, model, engine := workflowResultProducer(payload, graph)
	if agentID != "player" || model != "player-model" || engine != "loom" {
		t.Fatalf("result producer = (%q,%q,%q), want actual worker source", agentID, model, engine)
	}
	graph.Nodes[2].Config = machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueRunInput}}
	if agentID, model, engine = workflowResultProducer(payload, graph); agentID != "" || model != "" || engine != "" {
		t.Fatalf("input-sourced result was attributed to an agent: (%q,%q,%q)", agentID, model, engine)
	}
	graph.Nodes = append(graph.Nodes, machine.Node{ID: "combine", Type: machine.NodeTransform, Config: machine.TransformConfig{
		Operation: machine.TransformObject, Fields: map[string]machine.ValueRef{
			"captain": {Source: machine.ValueNodeOutput, NodeID: "captain"},
			"player":  {Source: machine.ValueNodeOutput, NodeID: "player"},
		},
	}})
	graph.Nodes[2].Config = machine.DeliverConfig{Result: machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "combine"}}
	if agentID, model, engine = workflowResultProducer(payload, graph); agentID != "" || model != "" || engine != "" {
		t.Fatalf("multi-agent result selected an arbitrary producer: (%q,%q,%q)", agentID, model, engine)
	}
}
