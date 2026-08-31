package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestRunActivityPublishedMembersBindsFrozenExecutionFacts(t *testing.T) {
	payload := frozen.ArtifactPayloadV1{
		Team: frozen.ArtifactTeamV1{
			LeadAgentID: "lead-1",
			Workers: []frozen.FrozenTeamWorker{{
				WorkerAgentID: "worker-1",
				Duty:          "verify assumptions",
			}},
		},
		Bundles: []frozen.FrozenExecutionBundle{
			{
				Agent: frozen.FrozenAgentRecord{
					AgentID: "lead-1", DisplayName: "Lead", Engine: "codex",
				},
			},
			{
				Agent: frozen.FrozenAgentRecord{
					AgentID: "worker-1", DisplayName: "Verifier", Engine: "claude-code",
				},
				Runtime:      &frozen.FrozenRuntimeBinding{RuntimeID: "runtime-1", Engine: "claude-code"},
				PrimaryModel: frozen.FrozenModelBinding{ProviderID: "provider-1", ModelID: "model-1"},
			},
		},
	}
	graph := machine.GraphDefinition{Nodes: []machine.Node{
		{ID: "lead", Label: "Plan", Type: machine.NodeLead, Config: machine.LeadConfig{}},
		{
			ID: "verify", Label: "Verify", Type: machine.NodeWorker,
			Config: machine.WorkerConfig{AgentID: "worker-1"},
			Inputs: map[string]machine.InputBinding{
				"facts": {
					ExpectedType: machine.ValueText,
					Value:        machine.ValueRef{Source: machine.ValueNodeOutput, NodeID: "lead", Path: "$.facts"},
				},
			},
		},
	}}
	members, runtimes := runActivityPublishedMembers(payload, graph, teamrun.StatusSucceeded, []runActivityDeliverableRef{
		{ID: "deliverable-1", NodeID: "verify"},
	})
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2", len(members))
	}
	worker := members[1]
	if worker.AgentID != "worker-1" || worker.Name != "Verifier" || worker.Status != "completed" {
		t.Fatalf("worker identity/status = %#v", worker)
	}
	if worker.Runtime == nil || worker.Runtime.RuntimeID != "runtime-1" ||
		worker.Runtime.Provider != "provider-1" || worker.Runtime.Model != "model-1" {
		t.Fatalf("worker runtime = %#v", worker.Runtime)
	}
	if len(worker.Stages) != 1 || len(worker.Stages[0].Inputs) != 1 ||
		worker.Stages[0].Inputs[0].Source != "node_output" ||
		worker.Stages[0].Inputs[0].NodeID != "lead" ||
		len(worker.Stages[0].OutputRefs) != 1 || worker.Stages[0].OutputRefs[0] != "deliverable-1" {
		t.Fatalf("worker stage = %#v", worker.Stages)
	}
	if len(runtimes) != 2 || runtimes[1].RuntimeID != "runtime-1" || runtimes[1].Status != "succeeded" {
		t.Fatalf("runtimes = %#v", runtimes)
	}
}

func TestApplyRunActivityEventsProjectsInputTimingAndToolTrace(t *testing.T) {
	members := []runActivityMember{{AgentID: "worker-1", Status: "pending", Stages: []runActivityMemberStage{{
		NodeID: "verify", Status: "pending", Inputs: []runActivityMemberInputRef{{Name: "facts"}}, Tools: []runActivityTool{},
	}}}}
	started := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	toolStarted := started.Add(time.Second)
	toolDone := started.Add(2 * time.Second)
	completed := started.Add(4 * time.Second)
	events := []teamrun.ActivityEvent{
		{Kind: "member_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: started,
			Detail: json.RawMessage(`{"input_summary":{"facts":"{\"source\":\"baseline\"}"}}`)},
		{Kind: "tool_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: toolStarted,
			Detail: json.RawMessage(`{"tool_name":"evidence_lookup","tool_call_id":"call-1","status":"ok"}`)},
		{Kind: "tool_completed", MemberID: "worker-1", NodeID: "verify", OccurredAt: toolDone,
			Detail: json.RawMessage(`{"tool_name":"evidence_lookup","tool_call_id":"call-1","status":"ok"}`)},
		{Kind: "member_completed", MemberID: "worker-1", NodeID: "verify", OccurredAt: completed,
			Detail: json.RawMessage(`{"duration_ms":4000,"tool_calls":1}`)},
	}
	applyRunActivityEvents(members, events)
	stage := members[0].Stages[0]
	if members[0].Status != "completed" || stage.Status != "completed" || stage.DurationMs != 4000 || stage.ToolCalls != 1 {
		t.Fatalf("member/stage status = %#v", members[0])
	}
	if stage.StartedAt == nil || !stage.StartedAt.Equal(started) || stage.CompletedAt == nil || !stage.CompletedAt.Equal(completed) {
		t.Fatalf("stage times = %#v", stage)
	}
	if stage.Inputs[0].Summary != `{"source":"baseline"}` {
		t.Fatalf("input summary = %q", stage.Inputs[0].Summary)
	}
	if len(stage.Tools) != 1 || stage.Tools[0].Name != "evidence_lookup" || stage.Tools[0].Status != "ok" || stage.Tools[0].CompletedAt == nil {
		t.Fatalf("tool trace = %#v", stage.Tools)
	}
}

func TestApplyRunActivityEventsResetsTerminalTimingWhenStageRestarts(t *testing.T) {
	members := []runActivityMember{{AgentID: "worker-1", Status: "pending", Stages: []runActivityMemberStage{{
		NodeID: "verify", Status: "pending", Inputs: []runActivityMemberInputRef{{Name: "facts"}}, Tools: []runActivityTool{},
	}}}}
	started := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	restarted := started.Add(10 * time.Second)
	events := []teamrun.ActivityEvent{
		{Kind: "member_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: started,
			Detail: json.RawMessage(`{"input_summary":{"facts":"first"}}`)},
		{Kind: "tool_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: started.Add(time.Second),
			Detail: json.RawMessage(`{"tool_name":"lookup","tool_call_id":"call-1"}`)},
		{Kind: "member_completed", MemberID: "worker-1", NodeID: "verify", OccurredAt: started.Add(4 * time.Second),
			Detail: json.RawMessage(`{"duration_ms":4000,"tool_calls":1}`)},
		{Kind: "member_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: restarted,
			Detail: json.RawMessage(`{"input_summary":{"facts":"corrected"}}`)},
	}
	applyRunActivityEvents(members, events)
	stage := members[0].Stages[0]
	if members[0].Status != "running" || stage.Status != "running" || stage.DurationMs != 0 || stage.ToolCalls != 0 {
		t.Fatalf("member/stage status after restart = %#v", members[0])
	}
	if stage.StartedAt == nil || !stage.StartedAt.Equal(restarted) || stage.CompletedAt != nil || len(stage.Tools) != 0 {
		t.Fatalf("stage timing/tools after restart = %#v", stage)
	}
	if stage.Inputs[0].Summary != "corrected" {
		t.Fatalf("input summary after restart = %q", stage.Inputs[0].Summary)
	}
}

func TestLatestRunActivityStageUsesNewestMemberBoundary(t *testing.T) {
	first := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	latest := first.Add(2 * time.Minute)
	members := []runActivityMember{
		{Stages: []runActivityMemberStage{{Name: "证据分析", CompletedAt: &first}}},
		{Stages: []runActivityMemberStage{{Name: "独立否定审查", CompletedAt: &latest}}},
	}
	if got := latestRunActivityStage(members, nil); got != "独立否定审查" {
		t.Fatalf("latest stage = %q", got)
	}
	if got := latestRunActivityStage(nil, []runActivityStage{{Name: "任务定义"}, {Name: "交付"}}); got != "交付" {
		t.Fatalf("fallback stage = %q", got)
	}
}

func TestRunActivityPublishedMembersOmitsConfiguredOnlyLead(t *testing.T) {
	payload := frozen.ArtifactPayloadV1{
		Team: frozen.ArtifactTeamV1{
			LeadAgentID: "lead-config-only",
			Workers:     []frozen.FrozenTeamWorker{{WorkerAgentID: "worker-1"}},
		},
	}
	graph := machine.GraphDefinition{Nodes: []machine.Node{{
		ID: "work", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "worker-1"},
	}}}
	members, _ := runActivityPublishedMembers(payload, graph, teamrun.StatusRunning, nil)
	if len(members) != 1 || members[0].AgentID != "worker-1" {
		t.Fatalf("members = %#v, want only executing worker", members)
	}
}
