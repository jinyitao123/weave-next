package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestDecisionTraceProjectsRunEvidenceOntoModelNodes(t *testing.T) {
	payload := frozen.ArtifactPayloadV1{GraphDefinition: json.RawMessage(councilGraph), Bundles: []frozen.FrozenExecutionBundle{decisionBundle("scout", "worker"), decisionBundle("attack", "worker"), decisionBundle("captain", "worker")},
		Team: frozen.ArtifactTeamV1{Workers: []frozen.FrozenTeamWorker{{WorkerAgentID: "scout", Duty: "assess"}, {WorkerAgentID: "attack", Duty: "propose"}, {WorkerAgentID: "captain", Duty: "decide"}}}}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		t.Fatalf("fixture graph: %+v", report.Issues)
	}
	start := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	events := []teamrun.ActivityEvent{{Seq: 1, Kind: "member_started", NodeID: "scout", MemberID: "scout", OccurredAt: start, Detail: json.RawMessage(`{}`)}, {Seq: 2, Kind: "member_started", NodeID: "attack", MemberID: "attack", OccurredAt: start, Detail: json.RawMessage(`{}`)}, {Seq: 3, Kind: "member_completed", NodeID: "scout", MemberID: "scout", OccurredAt: start.Add(900 * time.Millisecond), Detail: json.RawMessage(`{"duration_ms":900}`)}}
	items := []deliverable.FinalDeliverable{{RunSnapshotID: "snap", Content: `{"note":"two high cards left"}`, Metadata: json.RawMessage(`{"artifact_kind":"stage","node_id":"scout","node_type":"worker","source":"published_workflow"}`)},
		{RunSnapshotID: "other", Content: `{"note":"stale"}`, Metadata: json.RawMessage(`{"artifact_kind":"stage","node_id":"attack","node_type":"worker","source":"published_workflow"}`)}}
	trace := decisionTrace(payload, graph, teamrun.TeamRun{RunSnapshotID: "snap", Status: teamrun.StatusRunning}, items, events)
	nodes := trace["nodes"].([]decisionTraceNode)
	if len(nodes) != 3 || nodes[0].NodeID != "scout" || nodes[2].NodeID != "captain" {
		t.Fatalf("model nodes not in graph order: %+v", nodes)
	}
	if nodes[0].Status != "succeeded" || nodes[0].DurationMs != 900 || string(nodes[0].Output) != `{"note":"two high cards left"}` || nodes[0].Label != "Scout" {
		t.Fatalf("completed node wrong: %+v", nodes[0])
	}
	if nodes[1].Status != "running" || nodes[1].Output != nil || nodes[2].Status != "pending" {
		t.Fatalf("running node exposed another snapshot's output or unstarted node advanced: %+v", nodes[1:])
	}
	cancelled := decisionTrace(payload, graph, teamrun.TeamRun{RunSnapshotID: "snap", Status: teamrun.StatusCancelled}, items, events)["nodes"].([]decisionTraceNode)
	if cancelled[0].Status != "succeeded" || cancelled[1].Status != "cancelled" || cancelled[2].Status != "cancelled" {
		t.Fatalf("cancelled run statuses wrong: %+v", cancelled)
	}
}
