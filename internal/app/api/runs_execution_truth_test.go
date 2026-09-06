package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

func TestRunActivityDoesNotPromoteIntermediateFilesToFinal(t *testing.T) {
	items := []deliverable.FinalDeliverable{
		{ID: "lead-file", Metadata: json.RawMessage(`{"source":"published_workflow","artifact_kind":"final","node_type":"lead","node_id":"plan"}`)},
		{ID: "worker-file", Metadata: json.RawMessage(`{"source":"published_workflow","artifact_kind":"final","node_type":"worker","node_id":"review"}`)},
		{ID: "delivery", Metadata: json.RawMessage(`{"source":"published_workflow","artifact_kind":"final","node_type":"deliver","node_id":"send"}`)},
		{ID: "declared", Metadata: json.RawMessage(`{"artifact_kind":"final"}`)},
	}
	refs, _ := runActivityDeliverables(items)
	for i, want := range []string{"stage", "stage", "final", "final"} {
		if refs[i].Kind != want {
			t.Errorf("%s: kind=%s want=%s", refs[i].ID, refs[i].Kind, want)
		}
	}
}

func TestQueuedCLIMemberWaitsForPhysicalExecution(t *testing.T) {
	enqueued := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	claimed := enqueued.Add(time.Minute)
	for _, tt := range []struct{ taskStatus, stageStatus, want string }{
		{"queued", "running", "pending"},
		{"running", "running", "running"},
		{"queued", "cancelled", "cancelled"},
		{"completed", "completed", "completed"},
	} {
		members := []runActivityMember{{AgentID: "worker", Stages: []runActivityMemberStage{{NodeID: "stage", Status: tt.stageStatus, StartedAt: &enqueued}}}}
		applyRunPublicEvents(members, nil, map[string]runPublicTask{"worker:stage": {ID: "task", Status: tt.taskStatus, StartedAt: &claimed}}, false)
		stage := members[0].Stages[0]
		if stage.Status != tt.want {
			t.Fatalf("%+v: stage=%+v", tt, stage)
		}
		if tt.want == "pending" && stage.StartedAt != nil {
			t.Fatal("queued member has execution start time")
		}
		if tt.want == "running" && !stage.StartedAt.Equal(claimed) {
			t.Fatal("execution duration includes queue wait")
		}
	}
}

func TestCompletedCLIReceiptSurvivesActivityWindowTruncation(t *testing.T) {
	started := time.Date(2026, 9, 6, 6, 0, 0, 0, time.UTC)
	completed := started.Add(time.Minute)
	for _, tt := range []struct {
		name, result, want string
		invalidated        bool
		startsCompleted    bool
	}{
		{"successful receipt", `{"status":"completed"}`, "completed", false, false},
		{"replayed completion timing", `{"status":"completed"}`, "completed", false, true},
		{"failed receipt", `{"status":"failed"}`, "pending", false, false},
		{"no receipt", `{}`, "pending", false, false},
		{"superseded by correction", `{"status":"completed"}`, "pending", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			initialStatus := "pending"
			if tt.startsCompleted {
				initialStatus = "completed" // stale completion is still in the window
			}
			members := []runActivityMember{{AgentID: "worker", Stages: []runActivityMemberStage{{NodeID: "stage", Status: initialStatus}}}}
			applyRunPublicEvents(members, nil, map[string]runPublicTask{"worker:stage": {
				ID: "task", Status: "completed", Result: json.RawMessage(tt.result), StartedAt: &started, CompletedAt: &completed, Invalidated: tt.invalidated,
			}}, true)
			stage := members[0].Stages[0]
			if stage.Status != tt.want {
				t.Fatalf("stage=%+v want=%s", stage, tt.want)
			}
			if tt.want == "completed" && (stage.CompletedAt == nil || stage.DurationMs != 60000) {
				t.Fatalf("completion timing missing: %+v", stage)
			}
			if tt.invalidated && stage.CurrentTaskID != "" {
				t.Fatalf("superseded attempt shown as current: %+v", stage)
			}
		})
	}
}
