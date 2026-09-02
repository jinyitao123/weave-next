package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

type recordingWorkflowOutputStore struct {
	outputs []deliverable.WorkflowOutput
}

func (s *recordingWorkflowOutputStore) RecordWorkflowOutput(_ context.Context, output deliverable.WorkflowOutput) error {
	s.outputs = append(s.outputs, output)
	return nil
}

func TestRuntimeEventsFromEngineExecObservations(t *testing.T) {
	result, err := json.Marshal(map[string]any{
		"events": []map[string]any{
			{"kind": "tool_call", "tool": "shell", "call_id": "call-1", "input": "pwd"},
			{"kind": "tool_result", "tool": "shell", "call_id": "call-1", "status": "ok", "output": "/tmp/work"},
			{"kind": "message", "tool": "ignored"},
			{"kind": "tool_result", "tool": "shell", "call_id": "call-1", "status": "ok", "output": "/tmp/work"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	events := runtimeEventsFromEngineExecObservations([]taskqueue.EngineExecObservation{{
		TaskID: "task-1", Result: result,
	}}, 10)
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].CallID != "task-1:call-1" || events[0].Kind != "tool_call" || events[0].Input != "pwd" {
		t.Fatalf("first event = %#v", events[0])
	}
	if events[1].CallID != "task-1:call-1" || events[1].Kind != "tool_result" || events[1].Output != "/tmp/work" {
		t.Fatalf("second event = %#v", events[1])
	}
	if got := runtimeEventsFromEngineExecObservations([]taskqueue.EngineExecObservation{{TaskID: "task-1", Result: result}}, 1); len(got) != 1 {
		t.Fatalf("limited events = %#v", got)
	}
}

func TestRecordWorkflowArtifactsProjectsFanoutFiles(t *testing.T) {
	store := &recordingWorkflowOutputStore{}
	runtime := &WorkflowSerialRuntime{OutputRecorder: store}
	run := TeamRun{WorkspaceID: "workspace-1", RunID: "run-1", RunSnapshotID: "snapshot-1"}
	owner := workflowArtifactOwner{
		NodeID: "research", NodeLabel: "Research", NodeType: "worker", AgentID: "agent-1",
	}
	var usage nodeUsageReport
	if err := json.Unmarshal([]byte(`{"Artifacts":[
		{"path":"model/results.json","content_type":"application/json","content":"{\"ok\":true}"},
		{"path":"drawings/concept.svg","content_type":"image/svg+xml","content":"<svg/>"}
	]}`), &usage); err != nil {
		t.Fatal(err)
	}

	if err := runtime.recordWorkflowArtifacts(context.Background(), run, owner, usage.Artifacts); err != nil {
		t.Fatal(err)
	}
	if len(store.outputs) != 2 {
		t.Fatalf("recorded outputs = %#v", store.outputs)
	}
	for index, output := range store.outputs {
		if output.WorkspaceID != run.WorkspaceID || output.RunID != run.RunID ||
			output.RunSnapshotID != run.RunSnapshotID || output.NodeID != owner.NodeID || !output.Final {
			t.Fatalf("output[%d] identity = %#v", index, output)
		}
		if output.Artifact == nil || output.Artifact.Path != usage.Artifacts[index].Path ||
			output.Artifact.ContentType != usage.Artifacts[index].ContentType ||
			output.Artifact.Content != usage.Artifacts[index].Content {
			t.Fatalf("output[%d] artifact = %#v", index, output.Artifact)
		}
	}
}
