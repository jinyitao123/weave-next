package loomruntime

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jinyitao123/loom/contract"
)

func toolEvidenceRecord(t *testing.T, input string, result any) []byte {
	t.Helper()
	value := map[string]any{"kind": "tool", "input": contract.ToolCall{ID: "same-call", Name: "test_tool", Args: input}}
	if result != nil {
		value["response"] = result
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMemberToolEvidencePreservesEmptyRecordedOutput(t *testing.T) {
	var evidence MemberToolEvidence
	if !decodeMemberToolEvidence(toolEvidenceRecord(t, `{}`, contract.ToolResult{CallID: "same-call", Content: ""}), &evidence) {
		t.Fatal("tool receipt was not decoded")
	}
	if evidence.Input != `{}` || evidence.InputState != "recorded" || evidence.Output != "" || evidence.OutputState != "recorded" || evidence.OutputBytes != 0 {
		t.Fatalf("empty recorded output = %#v", evidence)
	}
}

func TestMemberToolEvidenceRequiresExactReceiptIdentity(t *testing.T) {
	for _, result := range []any{nil, contract.ToolResult{CallID: "another-call", Content: "wrong"}, contract.ToolResult{CallID: "same-call", ToolName: "another-tool", Content: "wrong"}, map[string]any{"call_id": "same-call"}} {
		var evidence MemberToolEvidence
		if !decodeMemberToolEvidence(toolEvidenceRecord(t, `{}`, result), &evidence) || evidence.InputState != "recorded" || evidence.OutputState != "missing" || evidence.Output != "" || evidence.Status != "running" {
			t.Fatalf("invalid receipt inferred output: %#v", evidence)
		}
	}
	var model MemberToolEvidence
	if decodeMemberToolEvidence([]byte(`{"kind":"model","input":{"id":"same-call","name":"model","args":"{}"},"response":{"call_id":"same-call","content":"private model context"}}`), &model) {
		t.Fatal("model operation entered tool evidence")
	}
}

func TestMemberToolEvidenceReportsBoundedPayloads(t *testing.T) {
	value := strings.Repeat("中", MemberToolEvidenceMaxBytes)
	input, _ := json.Marshal(map[string]string{"value": value})
	var evidence MemberToolEvidence
	if !decodeMemberToolEvidence(toolEvidenceRecord(t, string(input), contract.ToolResult{CallID: "same-call", Content: value, IsError: true, StatePatch: map[string]any{"private": "must not appear"}}), &evidence) {
		t.Fatal("tool receipt was not decoded")
	}
	if evidence.Status != "error" || evidence.InputState != "truncated" || evidence.OutputState != "truncated" || evidence.InputBytes != len(input) || evidence.OutputBytes != len(value) || len(evidence.Input) > MemberToolEvidenceMaxBytes || len(evidence.Output) > MemberToolEvidenceMaxBytes || !utf8.ValidString(evidence.Input) || !utf8.ValidString(evidence.Output) {
		t.Fatalf("bounded payload = %#v", evidence)
	}
	if strings.Contains(evidence.Output, "must not appear") {
		t.Fatal("tool control-plane patch entered visible evidence")
	}
}
