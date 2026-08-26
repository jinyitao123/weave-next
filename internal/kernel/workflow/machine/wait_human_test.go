package machine

import (
	"encoding/json"
	"testing"
)

func TestDecodeHumanWaitConfigAndTimerDefault(t *testing.T) {
	raw := json.RawMessage(`{
		"schema_version":1,
		"entry_node_id":"review",
		"input_contract":{"type":"json"},
		"output_contract":{"type":"json"},
		"nodes":[
			{"id":"review","type":"wait","config":{
				"kind":"human",
				"resume_schema":{"type":"object","properties":{"decision":{"type":"string"}},"required":["decision"]},
				"task":{"title":"终审","instructions":"确认交付物","audience_ref":"editor"}
			}},
			{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"review","path":""}}}
		],
		"edges":[{"id":"review-success","from_node_id":"review","to_node_id":"deliver","route":"success"}]
	}`)
	graph, report := DecodeGraphDefinitionV1(raw)
	if report != nil {
		t.Fatalf("decode human wait: %+v", report.Issues)
	}
	wait := graph.Nodes[0].Config.(WaitConfig)
	if wait.EffectiveKind() != WaitKindHuman || wait.Task == nil || wait.Task.AudienceRef != "editor" {
		t.Fatalf("unexpected human wait config: %+v", wait)
	}

	timerRaw := json.RawMessage(`{
		"schema_version":1,"entry_node_id":"timer",
		"input_contract":{"type":"json"},"output_contract":{"type":"json"},
		"nodes":[
			{"id":"timer","type":"wait","config":{"resume_schema":{"type":"object"}}},
			{"id":"deliver","type":"deliver","config":{"result":{"source":"node_output","node_id":"timer","path":""}}}
		],
		"edges":[{"id":"timer-success","from_node_id":"timer","to_node_id":"deliver","route":"success"}]
	}`)
	timerGraph, report := DecodeGraphDefinitionV1(timerRaw)
	if report != nil {
		t.Fatalf("decode legacy timer wait: %+v", report.Issues)
	}
	if got := timerGraph.Nodes[0].Config.(WaitConfig).EffectiveKind(); got != WaitKindTimer {
		t.Fatalf("legacy wait kind = %q, want timer", got)
	}
}

func TestHumanWaitRequiresTask(t *testing.T) {
	raw := json.RawMessage(`{
		"schema_version":1,"entry_node_id":"review",
		"input_contract":{"type":"json"},"output_contract":{"type":"json"},
		"nodes":[{"id":"review","type":"wait","config":{"kind":"human","resume_schema":{"type":"object"}}}],
		"edges":[]
	}`)
	_, report := DecodeGraphDefinitionV1(raw)
	if report == nil || len(report.Issues) != 1 || report.Issues[0].Path != "/nodes/0/config/task" {
		t.Fatalf("expected required task issue, got %+v", report)
	}
}

func TestHumanWaitInsideFanoutIsRejected(t *testing.T) {
	graph := GraphDefinition{
		SchemaVersion: SchemaVersionV1,
		EntryNodeID:   "parallel",
		Nodes: []Node{
			{ID: "parallel", Type: NodeParallel, Config: ParallelConfig{JoinNodeID: "join"}},
			{ID: "review", Type: NodeWait, Config: WaitConfig{
				Kind: WaitKindHuman, ResumeSchema: json.RawMessage(`{"type":"object"}`),
				Task: &HumanTaskConfig{Title: "终审", Instructions: "确认"},
			}},
			{ID: "worker", Type: NodeWorker, Config: WorkerConfig{Kind: WorkerDispatch}},
			{ID: "join", Type: NodeJoin, Config: JoinConfig{Policy: JoinAllSuccess}},
			{ID: "deliver", Type: NodeDeliver, Config: DeliverConfig{}},
		},
		Edges: []Edge{
			{ID: "branch-review", FromNodeID: "parallel", ToNodeID: "review", Route: RouteBranch},
			{ID: "branch-worker", FromNodeID: "parallel", ToNodeID: "worker", Route: RouteBranch},
			{ID: "review-join", FromNodeID: "review", ToNodeID: "join", Route: RouteSuccess},
			{ID: "worker-join", FromNodeID: "worker", ToNodeID: "join", Route: RouteJoin},
			{ID: "join-deliver", FromNodeID: "join", ToNodeID: "deliver", Route: RouteSuccess},
		},
	}
	report := validateTopology(ValidationContext{Graph: graph})
	for _, issue := range report.Issues {
		if issue.Code == CodeHumanWaitInFanout && issue.NodeID == "review" {
			return
		}
	}
	t.Fatalf("expected %s, got %+v", CodeHumanWaitInFanout, report.Issues)
}

func TestHumanWaitWithDeadlineRequiresTimeoutEdge(t *testing.T) {
	timeout := int64(60)
	graph := GraphDefinition{
		SchemaVersion: SchemaVersionV1, EntryNodeID: "review",
		Nodes: []Node{
			{ID: "review", Type: NodeWait, Config: WaitConfig{
				Kind: WaitKindHuman, ResumeSchema: json.RawMessage(`{"type":"object"}`),
				TimeoutSeconds: &timeout, Task: &HumanTaskConfig{Title: "终审", Instructions: "确认"},
			}},
			{ID: "deliver", Type: NodeDeliver, Config: DeliverConfig{}},
		},
		Edges: []Edge{{ID: "success", FromNodeID: "review", ToNodeID: "deliver", Route: RouteSuccess}},
	}
	report := validateTopology(ValidationContext{Graph: graph})
	for _, issue := range report.Issues {
		if issue.Code == CodeEdgeCardinalityInvalid && issue.NodeID == "review" {
			return
		}
	}
	t.Fatalf("expected missing timeout edge rejection, got %+v", report.Issues)
}
