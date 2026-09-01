package teamrun

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/taskqueue"
)

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
