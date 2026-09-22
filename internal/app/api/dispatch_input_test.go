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

func TestAuthorizedBusinessActionsRequireAnExplicitPublishedSubset(t *testing.T) {
	published := []string{
		"forge:action:sales_contract.ContractSubmit",
		"forge:action:sales_contract.RequestRevision",
	}
	if _, err := authorizedBusinessActions(published, nil); err == nil {
		t.Fatal("missing task scope was accepted for a workflow with business actions")
	}
	empty := []string{}
	if got, err := authorizedBusinessActions(published, &empty); err != nil || len(got) != 0 {
		t.Fatalf("material-only scope=%v err=%v", got, err)
	}
	requested := []string{published[1], published[0]}
	got, err := authorizedBusinessActions(published, &requested)
	if err != nil || len(got) != 2 || got[0] != published[0] || got[1] != published[1] {
		t.Fatalf("authorized scope=%v err=%v", got, err)
	}
	outside := []string{"forge:action:sales_contract.Delete"}
	if _, err := authorizedBusinessActions(published, &outside); err == nil {
		t.Fatal("action outside the published workflow was accepted")
	}
	duplicate := []string{published[0], published[0]}
	if _, err := authorizedBusinessActions(published, &duplicate); err == nil {
		t.Fatal("duplicate action scope was accepted")
	}
}

func TestAuthorizedBusinessActionsAllowOmissionWhenWorkflowHasNone(t *testing.T) {
	got, err := authorizedBusinessActions(nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("scope=%v err=%v", got, err)
	}
}

func TestEmptyBusinessActionScopeDoesNotRequireDelegationWithoutResources(t *testing.T) {
	if !ensurePreparedActions(nil, nil) {
		t.Fatal("empty business action scope unexpectedly required a delegation")
	}
	if ensurePreparedActions(nil, []string{"forge:action:sales_contract.ContractSubmit"}) {
		t.Fatal("non-empty business action scope was accepted without a delegation")
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
