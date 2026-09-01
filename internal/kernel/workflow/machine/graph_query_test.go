package machine

import "testing"

func TestOwningParallelNode(t *testing.T) {
	graph := GraphDefinition{
		Nodes: []Node{
			{ID: "fanout", Type: NodeParallel, Config: ParallelConfig{JoinNodeID: "join"}},
			{ID: "orders", Type: NodeWorker, Config: WorkerConfig{AgentID: "orders-agent", AgentVersion: 1}},
			{ID: "audit", Type: NodeWorker, Config: WorkerConfig{AgentID: "audit-agent", AgentVersion: 1}},
			{ID: "join", Type: NodeJoin, Config: JoinConfig{Policy: JoinAllSuccess}},
		},
		Edges: []Edge{
			{ID: "fanout-orders", FromNodeID: "fanout", ToNodeID: "orders", Route: RouteBranch},
			{ID: "fanout-audit", FromNodeID: "fanout", ToNodeID: "audit", Route: RouteBranch},
			{ID: "orders-join", FromNodeID: "orders", ToNodeID: "join", Route: RouteJoin},
			{ID: "audit-join", FromNodeID: "audit", ToNodeID: "join", Route: RouteJoin},
		},
	}
	if got := OwningParallelNode(graph, "join", []string{"audit"}); got != "fanout" {
		t.Fatalf("owning parallel = %q, want fanout", got)
	}
	if got := OwningParallelNode(graph, "join", []string{"outside"}); got != "" {
		t.Fatalf("unrelated candidate returned %q", got)
	}
}
