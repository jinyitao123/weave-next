package teamforge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

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
