package teamrun

import (
	"encoding/json"
	"strings"
	"testing"
)

func actionEvent(seq int64, kind, node, member, invocation, call, label, status string) ActivityEvent {
	detail, _ := json.Marshal(map[string]string{
		"source": "forge_mcp.run_action", "invocation_id": invocation, "tool_call_id": call,
		"action_label": label, "action_name": "action_" + call, "status": status,
	})
	return ActivityEvent{Seq: seq, Kind: kind, NodeID: node, MemberID: member, Detail: detail}
}

func TestClassifyRunBusinessResultPrecedence(t *testing.T) {
	failed := BusinessActionCounts{Total: 2, Succeeded: 1, Failed: 1}
	unknown := BusinessActionCounts{Total: 2, Succeeded: 1, Unknown: 1}
	clean := BusinessActionCounts{Total: 1, Succeeded: 1}
	for _, tc := range []struct {
		name, status, disposition string
		counts                    BusinessActionCounts
		want                      RunBusinessResult
	}{
		{"failure outranks a complete team result", "succeeded", "complete", failed, RunBusinessResultActionFailed},
		{"failure outranks needs input", "succeeded", "needs_input", failed, RunBusinessResultActionFailed},
		{"failure on a failed run is still an action failure", "failed", "", failed, RunBusinessResultActionFailed},
		{"unknown outranks a complete team result", "succeeded", "complete", unknown, RunBusinessResultActionUnknown},
		{"failure outranks unknown", "succeeded", "", BusinessActionCounts{Failed: 1, Unknown: 1}, RunBusinessResultActionFailed},
		{"needs input without successful actions", "succeeded", "needs_input", BusinessActionCounts{}, RunBusinessResultNeedsInput},
		{"successful receipt outranks model missing items", "succeeded", "needs_input", clean, RunBusinessResultCompleted},
		{"completed with recorded actions", "succeeded", "complete", clean, RunBusinessResultCompleted},
		{"completed with no action calls", "succeeded", "", BusinessActionCounts{}, RunBusinessResultCompleted},
		{"a failed run without actions has no business result", "failed", "", BusinessActionCounts{}, RunBusinessResultNone},
		{"a cancelled run has none", "cancelled", "needs_input", BusinessActionCounts{}, RunBusinessResultNone},
		{"a running run has none", "running", "", clean, RunBusinessResultNone},
	} {
		if got := ClassifyRunBusinessResult(tc.status, tc.disposition, tc.counts); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCountBusinessActionsIsLenientAndOrdered(t *testing.T) {
	events := []ActivityEvent{
		actionEvent(7, "business_action_result", "n", "m", "inv", "b", "", "failed"),
		actionEvent(1, "business_action_started", "n", "m", "inv", "a", "调价", ""),
		actionEvent(2, "business_action_result", "n", "m", "inv", "a", "", "succeeded"),
		actionEvent(5, "business_action_started", "n", "m", "inv", "b", "", ""),
		actionEvent(9, "business_action_started", "n", "m", "inv", "c", "提交", ""),
		{Seq: 10, Kind: "tool_call", Detail: json.RawMessage(`{"source":"forge_mcp.run_action"}`)},
		{Seq: 11, Kind: "business_action_started", NodeID: "n", MemberID: "m", Detail: json.RawMessage(`not json`)},
		{Seq: 12, Kind: "business_action_started", NodeID: "n", MemberID: "m", Detail: json.RawMessage(`{"source":"other","invocation_id":"x","tool_call_id":"x"}`)},
	}
	got := CountBusinessActions(events)
	if got.Total != 3 || got.Succeeded != 1 || got.Failed != 1 || got.Unknown != 1 {
		t.Fatalf("counts = %+v", got)
	}
	want := "业务动作“调价”调用返回成功。；业务动作“action_b”调用返回失败，请先核对业务记录后再处理。；业务动作“提交”结果未知，请先核对业务记录后再处理。"
	if got.Summary != want {
		t.Fatalf("summary = %q\nwant      %q", got.Summary, want)
	}
	if !got.NeedsVerification() || (BusinessActionCounts{Total: 1, Succeeded: 1}).NeedsVerification() {
		t.Fatal("NeedsVerification is wrong")
	}
}

func TestCountBusinessActionsRestartedCallKeepsLatestStartAndResult(t *testing.T) {
	events := []ActivityEvent{
		actionEvent(1, "business_action_started", "n", "m", "inv", "a", "首次", ""),
		actionEvent(2, "business_action_result", "n", "m", "inv", "a", "", "unknown"),
		actionEvent(3, "business_action_started", "n", "m", "inv", "a", "重试", ""),
		actionEvent(4, "business_action_result", "n", "m", "inv", "a", "", "succeeded"),
	}
	got := CountBusinessActions(events)
	if got.Total != 1 || got.Succeeded != 1 || got.Unknown != 0 || got.Summary != "业务动作“重试”调用返回成功。" {
		t.Fatalf("counts = %+v", got)
	}
}

func TestCountBusinessActionsBoundsLongLabels(t *testing.T) {
	label := strings.Repeat("长", 200)
	got := CountBusinessActions([]ActivityEvent{actionEvent(1, "business_action_started", "n", "m", "i", "c", label, "")})
	if want := "业务动作“" + strings.Repeat("长", 128) + "”结果未知，请先核对业务记录后再处理。"; got.Summary != want {
		t.Fatalf("summary = %q", got.Summary)
	}
}
