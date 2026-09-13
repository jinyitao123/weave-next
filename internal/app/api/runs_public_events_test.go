package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestPublicProgressMergesTerminalToolsAndNeverChangesStageOutcome(t *testing.T) {
	at := time.Now().UTC()
	members := []runActivityMember{{AgentID: "worker", Stages: []runActivityMemberStage{{NodeID: "research", Status: "completed", OutputRefs: []string{"saved"},
		Tools: []runActivityTool{{CallID: "task-old:call", Status: "ok"}, {CallID: "task-current:call", Status: "ok", Output: "observed result", CompletedAt: &at}}}}}}
	makeEvent := func(taskID string, seq int64, event engine.Event) teamrun.ActivityEvent {
		detail, _ := json.Marshal(map[string]any{"task_id": taskID, "task_seq": seq, "event": event})
		return teamrun.ActivityEvent{Kind: "runtime_public", MemberID: "worker", NodeID: "research", OccurredAt: at, Detail: detail}
	}
	events := []teamrun.ActivityEvent{
		makeEvent("task-current", 1, engine.Event{Kind: "tool_call", Tool: "shell", CallID: "call", Status: "running"}),
		makeEvent("task-current", 2, engine.Event{Kind: "tool_result", Tool: "shell", CallID: "call", Status: "ok", Output: "observed result"}),
		makeEvent("task-current", 3, engine.Event{Kind: "text", Text: "public answer"}),
		makeEvent("task-current", 4, engine.Event{Kind: "stream_end"}),
		makeEvent("task-old", 3, engine.Event{Kind: "text", Text: "old message arrived later"}),
	}
	current := map[string]runPublicTask{"worker:research": {ID: "task-current", Status: "completed"}}
	applyRunPublicEvents(members, events, current, false)
	stage := members[0].Stages[0]
	if stage.Status != "completed" || stage.PublicUpdatesState != "complete" || len(stage.PublicUpdates) != 1 || len(stage.Tools) != 1 || stage.Tools[0].Status != "ok" || stage.Tools[0].TaskID != "task-current" || stage.OutputRefs[0] != "saved" {
		t.Fatalf("public progress changed authoritative outcome or duplicated tool: %+v", stage)
	}
	current["worker:research"] = runPublicTask{ID: "task-current", Status: "completed", Result: json.RawMessage(`{"diagnostics":[{"code":"public_events_unavailable"}]}`)}
	applyRunPublicEvents(members, nil, current, false)
	if !members[0].Stages[0].PublicUpdatesTruncated || members[0].Stages[0].PublicUpdatesState != "partial" {
		t.Fatal("disk-failure diagnostic hidden")
	}
	applyRunPublicEvents(members, events, current, true)
	if !members[0].Stages[0].PublicUpdatesTruncated {
		t.Fatal("polling window clipping hidden")
	}
}

func TestPublicMessageSnapshotsReplaceOnlySameCLIItem(t *testing.T) {
	members := []runActivityMember{{AgentID: "worker", Stages: []runActivityMemberStage{{NodeID: "research", Status: "running"}}}}
	var events []teamrun.ActivityEvent
	for index, text := range []string{"a", "ab", "abc", "abc", "abc"} {
		itemID := "message-1"
		if index >= 3 {
			itemID = ""
		} // Missing IDs do not imply the same message.
		detail, _ := json.Marshal(map[string]any{"task_id": "task-current", "task_seq": index + 1, "event": engine.Event{Kind: "text", CallID: itemID, Text: text}})
		events = append(events, teamrun.ActivityEvent{Kind: "runtime_public", MemberID: "worker", NodeID: "research", Detail: detail})
	}
	applyRunPublicEvents(members, events, map[string]runPublicTask{"worker:research": {ID: "task-current", Status: "running"}}, false)
	updates := members[0].Stages[0].PublicUpdates
	if len(updates) != 3 || updates[0].Text != "abc" || updates[0].Seq != 3 || updates[1].Seq != 4 || updates[2].Seq != 5 {
		t.Fatalf("whole-message snapshots were appended or anonymous items collapsed: %+v", updates)
	}
}
