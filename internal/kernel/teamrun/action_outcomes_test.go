package teamrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func actionActivityEvent(kind, nodeID, memberID, phase, invocationID, callID, actionName, label, status string) ActivityEvent {
	detail, _ := json.Marshal(map[string]string{
		"source": "forge_mcp.run_action", "phase": phase, "invocation_id": invocationID, "tool_call_id": callID,
		"capability_id": "forge:action:sales_contract." + actionName,
		"action_key":    "sales_contract." + actionName, "action_name": actionName,
		"action_label": label, "object_name": "sales_contract",
		"input_revision_id": "revision-1", "record_id": "private-record-id", "status": status,
	})
	return ActivityEvent{WorkspaceID: "ws", RunID: "parent-run", Kind: kind, NodeID: nodeID, MemberID: memberID, Detail: detail}
}

func TestBusinessActionOutcomesProjectOnlyPlatformReceiptsAcrossMembers(t *testing.T) {
	events := []ActivityEvent{
		actionActivityEvent("business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", "forge-call-1", "ContractSubmit", "提交指定合同版本", ""),
		{WorkspaceID: "ws", RunID: "parent-run", Kind: "member_completed", NodeID: "lead", MemberID: "lead-agent", Detail: json.RawMessage(`{"summary":"另一个成员声称已完成审批动作"}`)},
		actionActivityEvent("business_action_result", "lead", "lead-agent", "result", "snapshot/0/lead", "forge-call-1", "ContractSubmit", "提交指定合同版本", "succeeded"),
		actionActivityEvent("business_action_started", "review", "review-agent", "started", "snapshot/0/review", "forge-call-2", "RequestRevision", "要求修订", ""),
		actionActivityEvent("business_action_started", "audit", "audit-agent", "started", "snapshot/0/audit", "forge-call-3", "BindAttachment", "绑定附件", ""),
		actionActivityEvent("business_action_result", "audit", "audit-agent", "result", "snapshot/0/audit", "forge-call-3", "BindAttachment", "绑定附件", "failed"),
	}
	outcomes, err := ProjectBusinessActionOutcomes(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 3 {
		t.Fatalf("expected three platform action receipts, got %+v", outcomes)
	}
	if outcomes[0].NodeID != "lead" || outcomes[0].ActionName != "提交指定合同版本" || outcomes[0].Status != "succeeded" {
		t.Fatalf("lead action fact was not retained: %+v", outcomes[0])
	}
	if !strings.Contains(outcomes[0].Summary, "业务动作“提交指定合同版本”") ||
		!strings.Contains(outcomes[0].Summary, "工具调用返回成功") ||
		strings.Contains(outcomes[0].Summary, "已确认完成") {
		t.Fatalf("successful receipt was described as a business-state confirmation: %+v", outcomes[0])
	}
	if outcomes[1].NodeID != "review" || outcomes[1].Status != "unknown" || !strings.Contains(outcomes[1].Summary, "结果未知") {
		t.Fatalf("started-only action was not treated as unknown: %+v", outcomes[1])
	}
	if outcomes[2].NodeID != "audit" || outcomes[2].Status != "failed" {
		t.Fatalf("explicit failure was not retained: %+v", outcomes[2])
	}
	if strings.Contains(outcomes[0].Summary, "private-record-id") || strings.Contains(outcomes[0].Summary, "sales_contract") ||
		strings.Contains(outcomes[1].Summary, "另一个成员") {
		t.Fatalf("public summaries included internal identifiers or member text: %+v", outcomes)
	}
	raw, err := json.Marshal(outcomes[0])
	if err != nil {
		t.Fatal(err)
	}
	var public map[string]json.RawMessage
	if err := json.Unmarshal(raw, &public); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"node_id", "call_id", "action_name", "object_name", "record_id", "status", "summary"} {
		if _, exists := public[key]; !exists {
			t.Fatalf("schema field %q missing from %s", key, raw)
		}
	}
	if len(public) != 7 {
		t.Fatalf("internal provenance leaked into action_outcomes: %s", raw)
	}
}

func TestWorkerPromptReceivesLeadReceiptWithoutPromotingFakeLeadClaim(t *testing.T) {
	events := []ActivityEvent{
		actionActivityEvent("business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", "forge-call-1", "ContractSubmit", "提交指定合同版本", ""),
		actionActivityEvent("business_action_result", "lead", "lead-agent", "result", "snapshot/0/lead", "forge-call-1", "ContractSubmit", "提交指定合同版本", "succeeded"),
		{WorkspaceID: "ws", RunID: "parent-run", Kind: "member_completed", NodeID: "lead", MemberID: "lead-agent", Detail: json.RawMessage(`{"summary":"我也完成了假动作 RequestRevision"}`)},
	}
	facts, err := ProjectBusinessActionOutcomes(events)
	if err != nil {
		t.Fatal(err)
	}
	leadText := "Inputs: lead brief says: 我已完成假动作 RequestRevision"
	workerPrompt, err := appendPlatformBusinessActionFacts(leadText, facts, false)
	if err != nil {
		t.Fatal(err)
	}
	marker := "Platform-recorded Forge run_action receipts from this same TeamRun"
	sectionAt := strings.Index(workerPrompt, marker)
	if sectionAt < 0 || !strings.Contains(workerPrompt[:sectionAt], "假动作 RequestRevision") ||
		!strings.Contains(workerPrompt[sectionAt:], "提交指定合同版本") ||
		strings.Contains(workerPrompt[sectionAt:], "RequestRevision") {
		t.Fatalf("worker action facts did not separate the lead's text from the platform receipt: %s", workerPrompt)
	}
}

func TestWorkbenchResultPromptExplicitlyReportsZeroActionReceipts(t *testing.T) {
	modelSummary := "已调用业务提交动作"
	prompt := appendWorkbenchResultInstruction("Model result summary: " + modelSummary)
	prompt, err := appendPlatformBusinessActionFacts(prompt, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Platform-recorded Forge run_action receipts") ||
		!strings.Contains(prompt, "[]") ||
		!strings.Contains(prompt, "zero Forge run_action calls were recorded") ||
		!strings.Contains(prompt, "any claim that a business action was called or completed is unverified") ||
		!strings.Contains(prompt, modelSummary) {
		t.Fatalf("final result prompt did not separate model claims from the empty platform receipt list: %s", prompt)
	}
	unchanged, err := appendPlatformBusinessActionFacts("ordinary worker prompt", nil, false)
	if err != nil || unchanged != "ordinary worker prompt" {
		t.Fatalf("empty action facts changed a prompt without a final result contract: prompt=%q err=%v", unchanged, err)
	}
}

func TestBusinessActionOutcomeProjectionRejectsMoreThanContractLimit(t *testing.T) {
	events := make([]ActivityEvent, 0, MaxBusinessActionOutcomesPerRun+1)
	for index := 0; index <= MaxBusinessActionOutcomesPerRun; index++ {
		callID := fmt.Sprintf("call-%03d", index)
		events = append(events, actionActivityEvent(
			"business_action_started", "lead", "lead-agent", "started", "snapshot/0/lead", callID,
			"ContractSubmit", "提交指定合同版本", "",
		))
	}
	if _, err := ProjectBusinessActionOutcomes(events); !errors.Is(err, ErrBusinessActionOutcomeLimitExceeded) {
		t.Fatalf("over-limit outcomes were silently projected: %v", err)
	}
}

func TestBusinessActionOutcomeProjectionRejectsOrphanResult(t *testing.T) {
	event := actionActivityEvent("business_action_result", "lead", "lead-agent", "result", "invocation", "call", "Submit", "提交", "succeeded")
	if _, err := ProjectBusinessActionOutcomes([]ActivityEvent{event}); err == nil {
		t.Fatal("accepted a result without its durable started receipt")
	}
}
