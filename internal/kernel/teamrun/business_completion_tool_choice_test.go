package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/businessaction"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

const completionAction = "forge:action:record.submit"

type scriptedCompletionLLM struct {
	requests  []contract.ChatRequest
	responses []contract.ChatResponse
}

func (l *scriptedCompletionLLM) Chat(_ context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	l.requests = append(l.requests, request)
	if len(l.responses) == 0 {
		return nil, fmt.Errorf("unexpected model call")
	}
	response := l.responses[0]
	l.responses = l.responses[1:]
	response.Usage = contract.Usage{InputTokens: 2, OutputTokens: 1}
	return &response, nil
}

func (*scriptedCompletionLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	panic("unexpected stream")
}

// receiptActionTools offers exactly the authorized action tool and records a
// successful platform receipt for each dispatch, like the real action ledger.
type receiptActionTools struct {
	name       string
	frame      *deliverycheck.BusinessReceiptFrame
	dispatched int
}

func (d *receiptActionTools) ListTools(context.Context) ([]contract.ToolDef, error) {
	return []contract.ToolDef{{Name: d.name, Description: "submit the record", InputSchema: json.RawMessage(`{"type":"object"}`)}}, nil
}

func (d *receiptActionTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	d.dispatched++
	d.frame.Receipts = append(d.frame.Receipts, deliverycheck.BusinessReceipt{WorkspaceID: "ws", RunID: "run", InputRevisionID: "input",
		CapabilityID: completionAction, ObjectName: "record", RecordID: "record-1", OperationID: fmt.Sprintf("effect-%d", d.dispatched), Status: "succeeded"})
	return &contract.ToolResult{CallID: call.ID, ToolName: call.Name, Content: `{"ok":true}`}, nil
}

func workbenchResult(t *testing.T, disposition, summary string, missing ...string) string {
	t.Helper()
	if missing == nil {
		missing = []string{}
	}
	raw, err := json.Marshal(machine.WorkbenchResultV1{Disposition: disposition, Summary: summary, MissingItems: missing})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func runCompletionNode(t *testing.T, llm *scriptedCompletionLLM) (*receiptActionTools, error) {
	t.Helper()
	g, frame := completionFixture()
	name, err := businessaction.CapabilityToolName(completionAction)
	if err != nil {
		t.Fatal(err)
	}
	tools := &receiptActionTools{name: name, frame: &frame}
	r := &WorkflowSerialRuntime{BusinessReceiptReader: func(context.Context, deliverable.Candidate) (deliverycheck.BusinessReceiptFrame, error) {
		copy := frame
		copy.Receipts = append([]deliverycheck.BusinessReceipt{}, frame.Receipts...)
		return copy, nil
	}}
	ctx, err := r.completionContext(TeamRun{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot"}, g)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.CompileAgent("ws", &registry.AgentRecord{Name: "writer", Model: "test", Compaction: &registry.CompactionConfig{Enabled: false}}, llm, tools, compiler.CompileOpts{})
	if err != nil {
		t.Fatal(err)
	}
	node := machine.Node{ID: "final", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "writer", AgentVersion: 1}, Output: &machine.OutputContract{Type: machine.ValueJSON, Schema: machine.WorkbenchResultSchemaV1()}}
	_, _, err = runAgentNode(ctx, node, frozen.ArtifactPayloadV1{}, map[string]workflow.RuntimeGraphEntry{runtimeEntryKey("writer", 1): {AgentID: "writer", AgentVersion: 1, Graph: compiled}}, "input", nil, nil, "", nil, nil, true)
	return tools, err
}

func TestRejectedCompletionClaimForcesTheMissingActionOnce(t *testing.T) {
	name, _ := businessaction.CapabilityToolName(completionAction)
	llm := &scriptedCompletionLLM{responses: []contract.ChatResponse{
		{Content: workbenchResult(t, "complete", "已转化"), StopReason: "stop"},
		{ToolCalls: []contract.ToolCall{{ID: "call-1", Name: name, Args: `{}`}}, StopReason: "tool_calls"},
		{Content: workbenchResult(t, "complete", "已转化"), StopReason: "stop"},
	}}
	tools, err := runCompletionNode(t, llm)
	if err != nil {
		t.Fatal(err)
	}
	if tools.dispatched != 1 || len(llm.requests) != 3 {
		t.Fatalf("dispatched=%d requests=%d", tools.dispatched, len(llm.requests))
	}
	if llm.requests[0].ToolChoice != nil || llm.requests[2].ToolChoice != nil {
		t.Fatalf("tool choice outside the corrected round: %#v / %#v", llm.requests[0].ToolChoice, llm.requests[2].ToolChoice)
	}
	if got := llm.requests[1].ToolChoice; got == nil || *got != (contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: name}) {
		t.Fatalf("corrected round tool choice = %#v", got)
	}
}

func TestForcedActionWithoutCallFailsExplicitly(t *testing.T) {
	llm := &scriptedCompletionLLM{responses: []contract.ChatResponse{
		{Content: workbenchResult(t, "complete", "已转化"), StopReason: "stop"},
		{Content: workbenchResult(t, "complete", "已转化（再次声称）"), StopReason: "stop"},
	}}
	tools, err := runCompletionNode(t, llm)
	var check *compiler.NodeCompletionCheckError
	if !errors.As(err, &check) || check.Reason != deliverycheck.ReasonRequiredActionNotCalled {
		t.Fatalf("err = %v", err)
	}
	if tools.dispatched != 0 || len(llm.requests) != 2 {
		t.Fatalf("dispatched=%d requests=%d", tools.dispatched, len(llm.requests))
	}
	if failure := ClassifyFailure(err); failure.Class != FailureClassVerification || failure.Retryable {
		t.Fatalf("forced-call failure classified as %+v", failure)
	}
}

func TestMissingInputIsReviewedOnceWithoutForcingAction(t *testing.T) {
	llm := &scriptedCompletionLLM{responses: []contract.ChatResponse{
		{Content: workbenchResult(t, "needs_input", "待核实", "缺少业务回执", "预计成交日期未知"), StopReason: "stop"},
		{Content: workbenchResult(t, "needs_input", "缺少客户确认的金额", "客户确认的金额"), StopReason: "stop"},
	}}
	tools, err := runCompletionNode(t, llm)
	if err != nil {
		t.Fatal(err)
	}
	if tools.dispatched != 0 || len(llm.requests) != 2 {
		t.Fatalf("dispatched=%d requests=%d", tools.dispatched, len(llm.requests))
	}
	if llm.requests[0].ToolChoice != nil {
		t.Fatal("the initial round was constrained")
	}
	if got := llm.requests[1].ToolChoice; got == nil || got.Mode != contract.ToolChoiceNone {
		t.Fatalf("missing-input review must forbid calls: %#v", got)
	}
	feedback := llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content
	if !strings.Contains(feedback, "optional fields left unknown") {
		t.Fatalf("review feedback = %q", feedback)
	}
}

func TestRequiredActionToolChoiceNamesOnlyALoneOfferedAction(t *testing.T) {
	name, _ := businessaction.CapabilityToolName(completionAction)
	other, _ := businessaction.CapabilityToolName("forge:action:record.other")
	action := contract.ToolDef{Name: name}
	read := contract.ToolDef{Name: "read_material", ReadOnly: true}
	for _, test := range []struct {
		name    string
		missing []string
		tools   []contract.ToolDef
		want    *contract.ToolChoice
	}{
		{"alone", []string{completionAction}, []contract.ToolDef{action}, &contract.ToolChoice{Mode: contract.ToolChoiceTool, Name: name}},
		{"reads stay possible", []string{completionAction}, []contract.ToolDef{read, action}, &contract.ToolChoice{Mode: contract.ToolChoiceRequired}},
		{"several missing", []string{completionAction, "forge:action:record.other"}, []contract.ToolDef{action, {Name: other}}, &contract.ToolChoice{Mode: contract.ToolChoiceRequired}},
		{"not offered here", []string{completionAction}, []contract.ToolDef{read}, nil},
		{"nothing missing", []string{}, []contract.ToolDef{action}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := requiredActionToolChoice(test.missing, test.tools)
			if (got == nil) != (test.want == nil) || (got != nil && *got != *test.want) {
				t.Fatalf("choice = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestBusinessInputReviewUsesStructuredRejectionHistory(t *testing.T) {
	if businessInputReviewed(nil) || businessInputReviewed([]string{deliverycheck.ReasonRequiredActionMissing, ""}) {
		t.Fatal("an unrelated rejection counted as the missing-input review")
	}
	if !businessInputReviewed([]string{deliverycheck.ReasonRequiredActionMissing, deliverycheck.ReasonInputReview}) {
		t.Fatal("the recorded missing-input review was not recognized")
	}
}

func TestMissingInputReviewRejectsToolCallBeforeDispatch(t *testing.T) {
	name, _ := businessaction.CapabilityToolName(completionAction)
	llm := &scriptedCompletionLLM{responses: []contract.ChatResponse{
		{Content: workbenchResult(t, "needs_input", "缺少真实材料", "技术协议PDF"), StopReason: "stop"},
		{ToolCalls: []contract.ToolCall{{ID: "incorrect-review-write", Name: name, Args: `{}`}}, StopReason: "tool_calls"},
	}}
	tools, err := runCompletionNode(t, llm)
	var check *compiler.NodeCompletionCheckError
	if !errors.As(err, &check) || check.Reason != "tool_call_forbidden_during_completion_review" {
		t.Fatalf("err = %v", err)
	}
	if tools.dispatched != 0 || len(tools.frame.Receipts) != 0 || len(llm.requests) != 2 {
		t.Fatalf("review dispatched a side effect: calls=%d receipts=%d rounds=%d", tools.dispatched, len(tools.frame.Receipts), len(llm.requests))
	}
	if failure := ClassifyFailure(err); failure.Class != FailureClassVerification || failure.Retryable {
		t.Fatalf("forbidden review call classified as %+v", failure)
	}
}

func TestReviewedCompletionStillRequiresActualReceipt(t *testing.T) {
	llm := &scriptedCompletionLLM{responses: []contract.ChatResponse{
		{Content: workbenchResult(t, "needs_input", "待核实", "缺少业务回执"), StopReason: "stop"},
		{Content: workbenchResult(t, "complete", "复核后仍声称完成"), StopReason: "stop"},
		{Content: workbenchResult(t, "complete", "未调用却声称完成"), StopReason: "stop"},
	}}
	tools, err := runCompletionNode(t, llm)
	var check *compiler.NodeCompletionCheckError
	if !errors.As(err, &check) || check.Reason != deliverycheck.ReasonRequiredActionNotCalled {
		t.Fatalf("err = %v", err)
	}
	if tools.dispatched != 0 || len(llm.requests) != 3 {
		t.Fatalf("calls=%d rounds=%d", tools.dispatched, len(llm.requests))
	}
	if got := llm.requests[1].ToolChoice; got == nil || got.Mode != contract.ToolChoiceNone {
		t.Fatalf("review choice = %#v", got)
	}
	if got := llm.requests[2].ToolChoice; got == nil || got.Mode != contract.ToolChoiceTool {
		t.Fatalf("missing-receipt choice = %#v", got)
	}
}
