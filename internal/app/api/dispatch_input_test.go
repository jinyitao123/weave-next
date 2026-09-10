package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDispatchInputSourceMessagesRejectAmbiguousProvenance(t *testing.T) {
	zero, one, negative := int64(0), int64(1), int64(-1)
	valid := dispatchInputSourceMessage{MessageID: "user-1", EventSeq: &zero, SHA256: strings.Repeat("a", 64)}
	if !validDispatchInputSourceMessages([]dispatchInputSourceMessage{valid, {MessageID: "user-2", EventSeq: &one, SHA256: strings.Repeat("b", 64)}}) {
		t.Fatal("valid ordered user source references rejected")
	}
	for _, test := range []struct {
		name     string
		messages []dispatchInputSourceMessage
	}{
		{"empty", nil},
		{"missing_sequence", []dispatchInputSourceMessage{{MessageID: "user-1", SHA256: valid.SHA256}}},
		{"negative_sequence", []dispatchInputSourceMessage{{MessageID: "user-1", EventSeq: &negative, SHA256: valid.SHA256}}},
		{"duplicate_sequence", []dispatchInputSourceMessage{valid, {MessageID: "user-2", EventSeq: &zero, SHA256: valid.SHA256}}},
		{"duplicate_message", []dispatchInputSourceMessage{valid, {MessageID: "user-1", EventSeq: &one, SHA256: valid.SHA256}}},
		{"invalid_hash", []dispatchInputSourceMessage{{MessageID: "user-1", EventSeq: &zero, SHA256: strings.Repeat("z", 64)}}},
		{"short_hash", []dispatchInputSourceMessage{{MessageID: "user-1", EventSeq: &zero, SHA256: "ab"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validDispatchInputSourceMessages(test.messages) {
				t.Fatal("ambiguous source references accepted")
			}
		})
	}
}

func TestBoundDispatchWirePreservesOmissionAndExactText(t *testing.T) {
	input := dispatchInputRevision{Task: " \n甲：“引号”\n", TeamID: "team", Mode: "workflow", WorkflowID: "flow", WorkflowVersion: 1}
	input.ClientRequestID = "fixed-key"
	for _, test := range []struct {
		name string
		body string
		want bool
	}{
		{"omitted", `{}`, true},
		{"exact", `{"task":" \n甲：“引号”\n"}`, true},
		{"trailing_newline_lost", `{"task":" \n甲：“引号”"}`, false},
		{"empty_assertion", `{"task":""}`, false},
		{"wrong_nonce", `{"client_request_id":"another-key"}`, false},
		{"empty_workflow_assertion", `{"workflow_id":""}`, false},
		{"different_version", `{"workflow_version":2}`, false},
		{"conversation_substitution", `{"conversation_id":"old-conversation"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire teamDispatchWireRequest
			if err := json.Unmarshal([]byte(test.body), &wire); err != nil {
				t.Fatal(err)
			}
			if got := wire.matchesInput("team", input); got != test.want {
				t.Fatalf("match=%v want=%v", got, test.want)
			}
		})
	}
	for _, body := range []string{`{"task":null}`, `{"client_request_id":null}`, `{"workflow_version":null}`} {
		var wire teamDispatchWireRequest
		if json.Unmarshal([]byte(body), &wire) == nil {
			t.Fatalf("explicit null was treated as omission: %s", body)
		}
	}
}
