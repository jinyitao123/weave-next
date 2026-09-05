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
			Detail: json.RawMessage(`{"tool_name":"evidence_lookup","tool_call_id":"call-1","status":"ok","input":"{\"query\":\"baseline\"}"}`)},
		{Kind: "tool_completed", MemberID: "worker-1", NodeID: "verify", OccurredAt: toolDone,
			Detail: json.RawMessage(`{"tool_name":"evidence_lookup","tool_call_id":"call-1","status":"ok","output":"2 results"}`)},
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
	if len(stage.Tools) != 1 || stage.Tools[0].Name != "evidence_lookup" || stage.Tools[0].Status != "ok" || stage.Tools[0].CompletedAt == nil ||
		stage.Tools[0].Input != `{"query":"baseline"}` || stage.Tools[0].Output != "2 results" {
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

func TestApplyRunActivityEventsProjectsAndClearsRetryableFailure(t *testing.T) {
	members := []runActivityMember{{AgentID: "worker-1", Status: "pending", Stages: []runActivityMemberStage{{
		NodeID: "verify", Status: "pending", Tools: []runActivityTool{},
	}}}}
	failed := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	applyRunActivityEvents(members, []teamrun.ActivityEvent{{
		Kind: "member_failed", MemberID: "worker-1", NodeID: "verify", OccurredAt: failed,
		Detail: json.RawMessage(`{"failure_class":"infrastructure","failure_reason":"runtime connection failed","retryable":true}`),
	}})
	stage := members[0].Stages[0]
	if stage.Status != "failed" || stage.FailureClass != "infrastructure" ||
		stage.FailureReason != "runtime connection failed" || !stage.Retryable {
		t.Fatalf("projected failure = %#v", stage)
	}
	applyRunActivityEvents(members, []teamrun.ActivityEvent{{
		Kind: "member_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: failed.Add(time.Second),
	}})
	stage = members[0].Stages[0]
	if stage.Status != "running" || stage.FailureClass != "" || stage.FailureReason != "" || stage.Retryable {
		t.Fatalf("restarted failure state = %#v", stage)
	}
}

func TestRefineRunActivityCompletenessMarksRecordedStageFactsComplete(t *testing.T) {
	started := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	completed := started.Add(4 * time.Second)
	members := []runActivityMember{{AgentID: "worker-1", Status: "pending", Stages: []runActivityMemberStage{{
		NodeID: "verify", Status: "pending",
		Inputs:     []runActivityMemberInputRef{{Name: "facts"}},
		OutputRefs: []string{"deliverable-1"},
		Tools:      []runActivityTool{},
	}}}}
	applyRunActivityEvents(members, []teamrun.ActivityEvent{
		{Kind: "member_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: started,
			Detail: json.RawMessage(`{"input_summary":{"facts":"baseline"}}`)},
		{Kind: "tool_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: started.Add(time.Second),
			Detail: json.RawMessage(`{"tool_name":"evidence_lookup","tool_call_id":"call-1"}`)},
		{Kind: "tool_completed", MemberID: "worker-1", NodeID: "verify", OccurredAt: started.Add(2 * time.Second),
			Detail: json.RawMessage(`{"tool_name":"evidence_lookup","tool_call_id":"call-1","status":"ok"}`)},
		{Kind: "member_completed", MemberID: "worker-1", NodeID: "verify", OccurredAt: completed,
			Detail: json.RawMessage(`{"duration_ms":4000,"tool_calls":1}`)},
	})
	completeness := map[string]string{
		"members": "complete", "activity_events": "complete", "deliverables": "complete",
		"stages": "partial", "member_inputs": "partial", "member_outputs": "partial", "member_tool_activity": "partial",
	}
	refineRunActivityCompleteness(completeness, members, teamrun.StatusSucceeded)
	for _, key := range []string{"stages", "member_inputs", "member_outputs", "member_tool_activity"} {
		if completeness[key] != "complete" {
			t.Fatalf("%s completeness = %q, want complete; members=%#v", key, completeness[key], members)
		}
	}
}

func TestRefineRunActivityCompletenessKeepsToolTracePartialWhenCompletionIsMissing(t *testing.T) {
	started := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	completed := started.Add(4 * time.Second)
	members := []runActivityMember{{AgentID: "worker-1", Status: "pending", Stages: []runActivityMemberStage{{
		NodeID: "verify", Status: "pending", OutputRefs: []string{"deliverable-1"}, Tools: []runActivityTool{},
	}}}}
	applyRunActivityEvents(members, []teamrun.ActivityEvent{
		{Kind: "member_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: started},
		{Kind: "tool_started", MemberID: "worker-1", NodeID: "verify", OccurredAt: started.Add(time.Second),
			Detail: json.RawMessage(`{"tool_name":"evidence_lookup","tool_call_id":"call-1"}`)},
		{Kind: "member_completed", MemberID: "worker-1", NodeID: "verify", OccurredAt: completed,
			Detail: json.RawMessage(`{"duration_ms":4000,"tool_calls":1}`)},
	})
	completeness := map[string]string{
		"members": "complete", "activity_events": "complete", "deliverables": "complete",
		"stages": "partial", "member_inputs": "partial", "member_outputs": "partial", "member_tool_activity": "partial",
	}
	refineRunActivityCompleteness(completeness, members, teamrun.StatusSucceeded)
	if completeness["member_tool_activity"] != "partial" {
		t.Fatalf("member_tool_activity completeness = %q, want partial", completeness["member_tool_activity"])
	}
	if completeness["stages"] != "partial" {
		t.Fatalf("stages completeness = %q, want partial", completeness["stages"])
	}
}

func TestRunActivityStageProgressCountsMemberStagesAndHumanWait(t *testing.T) {
	members := []runActivityMember{
		{Stages: []runActivityMemberStage{{NodeID: "research", Status: "completed"}, {NodeID: "draft", Status: "running"}}},
		{Stages: []runActivityMemberStage{{NodeID: "check", Status: "completed"}}},
	}
	stages := runActivityCurrentStages(members, []runActivityStage{{NodeID: "review", Status: "waiting"}})
	completed, total := runActivityStageProgress(stages)
	if completed != 2 || total != 4 {
		t.Fatalf("progress = %d/%d, want 2/4", completed, total)
	}
}

func TestRunActivityTruncatedWindowDoesNotClaimCompleteToolHistory(t *testing.T) {
	started := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
	members := []runActivityMember{{Stages: []runActivityMemberStage{{
		NodeID: "draft", Status: "completed", StartedAt: &started, OutputRefs: []string{"saved-draft"},
	}}}}
	completeness := map[string]string{
		"members": "complete", "activity_events": "partial", "deliverables": "complete",
		"stages": "partial", "member_inputs": "partial", "member_outputs": "partial", "member_tool_activity": "partial",
	}
	refineRunActivityCompleteness(completeness, members, teamrun.StatusSucceeded)
	if completeness["member_tool_activity"] != "partial" || completeness["stages"] != "partial" {
		t.Fatalf("truncated history claimed complete: %#v", completeness)
	}
}

func TestRunActivityProgressPreservesOutputsWithoutCompletingRework(t *testing.T) {
	for _, status := range []string{"running", "failed", "completed"} {
		t.Run(status, func(t *testing.T) {
			members := []runActivityMember{{AgentID: "writer", Stages: []runActivityMemberStage{
				{NodeID: "draft", Name: "Revised draft", Status: status, OutputRefs: []string{"saved-draft"}},
				{NodeID: "check", Status: "completed", OutputRefs: []string{"saved-check"}},
			}}}
			recorded := []runActivityStage{
				{NodeID: "draft", Name: "Old draft", Status: "completed"},
				{NodeID: "check", Status: "completed"},
				{NodeID: "transform", Status: "completed"},
				{NodeID: "deliver", Status: "completed"},
			}
			stages := runActivityCurrentStages(members, recorded)
			completed, total := runActivityStageProgress(stages)
			wantCompleted := 3
			if status == "completed" {
				wantCompleted = 4
			}
			if stages[0].Status != status || stages[0].Name != "Revised draft" || completed != wantCompleted || total != 4 {
				t.Fatalf("current stages = %#v, progress = %d/%d", stages, completed, total)
			}
			if members[0].Stages[0].OutputRefs[0] != "saved-draft" || recorded[0].Status != "completed" {
				t.Fatal("current progress changed retained output evidence")
			}
		})
	}
}

func TestRunActivityMemberSummaryDoesNotHideAnotherStagesFailure(t *testing.T) {
	for _, order := range [][]string{{"failed", "running"}, {"running", "failed"}, {"failed", "pending"}, {"pending", "failed"}} {
		members := []runActivityMember{{Status: "pending", Stages: []runActivityMemberStage{
			{NodeID: "draft", Status: order[0]}, {NodeID: "review", Status: order[1]},
		}}}
		summarizeRunActivityMembers(members)
		if members[0].Status != "failed" {
			t.Fatalf("failure hidden by stage order %v: %#v", order, members[0])
		}
	}
	members := []runActivityMember{{Status: "failed", Stages: []runActivityMemberStage{
		{NodeID: "draft", Status: "pending"}, {NodeID: "review", Status: "completed"},
	}}}
	summarizeRunActivityMembers(members)
	if members[0].Status != "partially_completed" {
		t.Fatalf("queued retry retained stale member failure: %#v", members[0])
	}
}

func TestRunActivityToolWindowShowsCurrentWorkUntilMemberFinishes(t *testing.T) {
	for _, previous := range []string{"completed", "failed"} {
		for _, kind := range []string{"tool_started", "tool_completed"} {
			t.Run(previous+"/"+kind, func(t *testing.T) {
				old := time.Date(2026, 8, 31, 8, 0, 0, 0, time.UTC)
				members := []runActivityMember{{AgentID: "writer", Status: previous, Stages: []runActivityMemberStage{{
					NodeID: "draft", Status: previous, OutputRefs: []string{"saved-draft"},
					StartedAt: &old, CompletedAt: &old, DurationMs: 40, ToolCalls: 2,
					FailureClass: "infrastructure", FailureReason: "old failure", Retryable: true,
				}}}}
				applyRunActivityEvents(members, []teamrun.ActivityEvent{{
					Kind: kind, MemberID: "writer", NodeID: "draft", OccurredAt: old.Add(time.Minute),
					Detail: json.RawMessage(`{"tool_name":"read","tool_call_id":"current-call","status":"ok"}`),
				}})
				stage := members[0].Stages[0]
				if stage.Status != "running" || members[0].Status != "running" || stage.CompletedAt != nil || stage.StartedAt != nil ||
					stage.DurationMs != 0 || stage.ToolCalls != 0 || stage.FailureClass != "" || stage.FailureReason != "" || stage.Retryable {
					t.Fatalf("stale lifecycle after current tool event: %#v", stage)
				}
				stages := runActivityCurrentStages(members, []runActivityStage{{NodeID: "draft", Status: "completed"}})
				if completed, total := runActivityStageProgress(stages); completed != 0 || total != 1 || stages[0].Status != "running" {
					t.Fatalf("rework reported complete: %#v %d/%d", stages, completed, total)
				}
				applyRunActivityEvents(members, []teamrun.ActivityEvent{{
					Kind: "member_completed", MemberID: "writer", NodeID: "draft", OccurredAt: old.Add(2 * time.Minute),
				}})
				if members[0].Status != "completed" || members[0].Stages[0].OutputRefs[0] != "saved-draft" {
					t.Fatal("completion or retained output was lost")
				}
			})
		}
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
	if got := latestRunActivityStage(nil, []runActivityStage{{Name: "起草", Status: "running"}, {Name: "交付", Status: "pending"}}); got != "起草" {
		t.Fatalf("upcoming stage replaced current work: %q", got)
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
