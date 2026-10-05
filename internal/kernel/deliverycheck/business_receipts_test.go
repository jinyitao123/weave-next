package deliverycheck

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

const receiptAction = "forge:action:record.submit"

func receiptScope() BusinessReceiptScope {
	return BusinessReceiptScope{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", InputRevisionID: "input", SubjectID: "employee", AllowedCapabilityIDs: []string{receiptAction}, ObjectName: "record", RecordID: "record-1"}
}

func TestBusinessReceiptSimulationSourceAndScopeAreIsolated(t *testing.T) {
	params := BusinessReceiptParameters{RequiredCapabilityIDs: []string{receiptAction}, WhenAuthorized: true, AllowNeedsInput: false}
	now := time.Now().UTC()
	scope := BusinessReceiptScope{
		WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", InputRevisionID: "input", SubjectID: "developer",
		AllowedCapabilityIDs: []string{receiptAction}, DevelopmentTrial: true, TerminalAt: &now,
	}
	simulated := BusinessReceipt{
		WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", SubjectID: "developer", InputRevisionID: "input",
		CapabilityID: receiptAction, ObjectName: "record", OperationID: "operation-1", Status: "succeeded", Simulated: true,
		OccurredAt: now.Add(-time.Second),
	}
	if got := EvaluateBusinessReceipts(params, scope, []BusinessReceipt{simulated}, BusinessReceiptResult{Disposition: "complete"}); !got.Accept || got.Result.Reason != "required_business_actions_recorded" {
		t.Fatalf("simulated trial receipt = %+v", got)
	}

	formal := scope
	formal.DevelopmentTrial = false
	formal.ObjectName, formal.RecordID = "record", "record-1"
	if got := EvaluateBusinessReceipts(params, formal, []BusinessReceipt{simulated}, BusinessReceiptResult{Disposition: "complete"}); !got.HardStop || got.Accept {
		t.Fatalf("formal path accepted a simulated receipt: %+v", got)
	}
	real := simulated
	real.Simulated = false
	real.RunSnapshotID, real.SubjectID = "", ""
	real.RecordID = "record-1"
	if got := EvaluateBusinessReceipts(params, scope, []BusinessReceipt{real}, BusinessReceiptResult{Disposition: "complete"}); !got.HardStop || got.Accept {
		t.Fatalf("trial path accepted a real Forge receipt: %+v", got)
	}

	wrongSnapshot := simulated
	wrongSnapshot.RunSnapshotID = "another-snapshot"
	if got := EvaluateBusinessReceipts(params, scope, []BusinessReceipt{wrongSnapshot}, BusinessReceiptResult{Disposition: "complete"}); !got.HardStop {
		t.Fatalf("trial borrowed another run snapshot: %+v", got)
	}
	wrongActor := simulated
	wrongActor.SubjectID = "another-developer"
	if got := EvaluateBusinessReceipts(params, scope, []BusinessReceipt{wrongActor}, BusinessReceiptResult{Disposition: "complete"}); !got.HardStop {
		t.Fatalf("trial borrowed another developer: %+v", got)
	}
	late := simulated
	late.OccurredAt = now.Add(time.Second)
	if got := EvaluateBusinessReceipts(params, scope, []BusinessReceipt{late}, BusinessReceiptResult{Disposition: "complete"}); !got.HardStop {
		t.Fatalf("terminal run accepted a late simulation receipt: %+v", got)
	}
}
func receipt(status string) BusinessReceipt {
	return BusinessReceipt{WorkspaceID: "ws", RunID: "run", InputRevisionID: "input", CapabilityID: receiptAction, ObjectName: "record", RecordID: "record-1", OperationID: "operation-1", Status: status}
}

func TestBusinessReceiptCompletionDecisionMatrix(t *testing.T) {
	params := BusinessReceiptParameters{RequiredCapabilityIDs: []string{receiptAction}, WhenAuthorized: true, AllowNeedsInput: true}
	for _, test := range []struct {
		name         string
		scope        func(*BusinessReceiptScope)
		receipts     []BusinessReceipt
		result       BusinessReceiptResult
		accept, hard bool
		reason       string
	}{
		{"no required call", nil, []BusinessReceipt{}, BusinessReceiptResult{Disposition: "complete"}, false, false, "required_business_action_missing"},
		{"matching call", nil, []BusinessReceipt{receipt("succeeded")}, BusinessReceiptResult{Disposition: "complete"}, true, false, "required_business_actions_recorded"},
		{"durable duplicate", nil, []BusinessReceipt{receipt("succeeded"), receipt("succeeded")}, BusinessReceiptResult{Disposition: "complete"}, true, false, "required_business_actions_recorded"},
		{"explicit read only", func(s *BusinessReceiptScope) { s.AllowedCapabilityIDs = []string{}; s.RecordID = ""; s.ObjectName = "" }, []BusinessReceipt{}, BusinessReceiptResult{Disposition: "complete"}, true, false, "business_actions_not_requested"},
		{"other permission is not required", func(s *BusinessReceiptScope) { s.AllowedCapabilityIDs = []string{"forge:action:record.other"} }, []BusinessReceipt{}, BusinessReceiptResult{Disposition: "complete"}, true, false, "business_actions_not_requested"},
		{"missing input before effects", nil, []BusinessReceipt{}, BusinessReceiptResult{Disposition: "needs_input", MissingItems: []string{"required file"}}, true, false, "business_actions_deferred_for_input"},
		{"cannot hide effect behind input", nil, []BusinessReceipt{receipt("succeeded")}, BusinessReceiptResult{Disposition: "needs_input", MissingItems: []string{"another file"}}, false, true, "business_actions_cannot_be_deferred"},
		{"failed stops", nil, []BusinessReceipt{receipt("failed")}, BusinessReceiptResult{Disposition: "complete"}, false, true, "business_action_failed"},
		{"unknown stops", nil, []BusinessReceipt{receipt("unknown")}, BusinessReceiptResult{Disposition: "needs_input", MissingItems: []string{"file"}}, false, true, "business_action_unknown"},
		{"missing receipt source", nil, nil, BusinessReceiptResult{Disposition: "complete"}, false, true, "business_receipt_evidence_unavailable"},
		{"missing scope", func(s *BusinessReceiptScope) { s.AllowedCapabilityIDs = nil }, []BusinessReceipt{}, BusinessReceiptResult{Disposition: "complete"}, false, true, "business_receipt_evidence_unavailable"},
		{"missing protected record", func(s *BusinessReceiptScope) { s.RecordID = "" }, []BusinessReceipt{}, BusinessReceiptResult{Disposition: "complete"}, false, true, "business_receipt_record_binding_missing"},
		{"cancelled", func(s *BusinessReceiptScope) { s.Closed = true }, []BusinessReceipt{}, BusinessReceiptResult{Disposition: "complete"}, false, true, "business_receipt_evidence_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := receiptScope()
			if test.scope != nil {
				test.scope(&s)
			}
			got := EvaluateBusinessReceipts(params, s, test.receipts, test.result)
			if got.Accept != test.accept || got.HardStop != test.hard || got.Result.Reason != test.reason {
				t.Fatalf("got=%+v", got)
			}
			if !got.Accept && !got.HardStop && !strings.Contains(got.Feedback, "Do not repeat actions") {
				t.Fatal("missing feedback could cause successful effects to be repeated")
			}
		})
	}
}

func TestBusinessReceiptsNeverBorrowAnotherIdentity(t *testing.T) {
	for _, field := range []string{"workspace", "run", "input", "record", "object", "capability", "operation"} {
		t.Run(field, func(t *testing.T) {
			r := receipt("succeeded")
			switch field {
			case "workspace":
				r.WorkspaceID = "other"
			case "run":
				r.RunID = "other"
			case "input":
				r.InputRevisionID = "other"
			case "record":
				r.RecordID = "other"
			case "object":
				r.ObjectName = "other"
			case "capability":
				r.CapabilityID = "forge:action:record.other"
			case "operation":
				r.OperationID = ""
			}
			got := EvaluateBusinessReceipts(BusinessReceiptParameters{RequiredCapabilityIDs: []string{receiptAction}, WhenAuthorized: true}, receiptScope(), []BusinessReceipt{r}, BusinessReceiptResult{Disposition: "complete"})
			if !got.HardStop || got.Accept {
				t.Fatalf("borrowed %s: %+v", field, got)
			}
		})
	}
}

func TestBusinessReceiptParameterAndContractValidation(t *testing.T) {
	valid := `{"required_capability_ids":["forge:action:record.submit"],"when_authorized":true,"allow_needs_input":true}`
	for _, raw := range []string{strings.Replace(valid, `true,"allow`, `false,"allow`, 1), strings.Replace(valid, `"allow_needs_input":true`, `"other":true`, 1), strings.Replace(valid, `["forge:action:record.submit"]`, `[]`, 1), strings.Replace(valid, `["forge:action:record.submit"]`, `["forge:action:record.submit","forge:action:record.submit"]`, 1), valid + ` {}`, strings.Replace(valid, `"when_authorized":true`, `"when_authorized":false,"when_authorized":true`, 1)} {
		if _, err := ParseBusinessReceiptParameters(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid parameters accepted: %s", raw)
		}
	}
	contract := &deliverable.DeliveryContract{ExternalEffects: deliverable.ExternalEffectsRequired, ExternalEffectsCheckID: "effects", RequiredChecks: []deliverable.CheckSpec{{ID: "effects", VerifierID: BusinessReceiptsID, VerifierVersion: BusinessReceiptsVersion, Parameters: json.RawMessage(valid)}}}
	if err := ValidateBusinessReceiptCapabilities(contract, []string{receiptAction}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBusinessReceiptCapabilities(contract, []string{}); err == nil {
		t.Fatal("unbound action accepted")
	}
	contract.RequiredChecks[0].VerifierVersion = "v2"
	if _, err := BusinessReceiptCheck(contract); err == nil {
		t.Fatal("unknown checker version accepted")
	}
}

func TestBusinessReceiptInputReviewHappensOnceAndNeverHidesEffects(t *testing.T) {
	params := BusinessReceiptParameters{RequiredCapabilityIDs: []string{receiptAction}, WhenAuthorized: true, AllowNeedsInput: true}
	needsInput := BusinessReceiptResult{Disposition: "needs_input", MissingItems: []string{"receipt"}}
	review := needsInput
	review.RequireInputReview = true
	first := EvaluateBusinessReceipts(params, receiptScope(), []BusinessReceipt{}, review)
	if first.Accept || first.HardStop || first.Result.Reason != ReasonInputReview || !strings.Contains(first.Feedback, "Only inputs that block those actions count as missing") || !strings.Contains(first.Feedback, "Do not repeat actions") {
		t.Fatalf("first needs_input = %+v", first)
	}
	if later := EvaluateBusinessReceipts(params, receiptScope(), []BusinessReceipt{}, needsInput); !later.Accept || later.Result.Reason != "business_actions_deferred_for_input" {
		t.Fatalf("reviewed needs_input = %+v", later)
	}
	strict := params
	strict.AllowNeedsInput = false
	if later := EvaluateBusinessReceipts(strict, receiptScope(), []BusinessReceipt{}, needsInput); !later.HardStop || later.Result.Reason != "business_actions_cannot_be_deferred" {
		t.Fatalf("strict reviewed needs_input = %+v", later)
	}
	if effect := EvaluateBusinessReceipts(params, receiptScope(), []BusinessReceipt{receipt("succeeded")}, review); !effect.HardStop || effect.Result.Reason != "business_actions_cannot_be_deferred" {
		t.Fatalf("review hid an existing effect: %+v", effect)
	}
	readOnly := receiptScope()
	readOnly.AllowedCapabilityIDs = []string{}
	if got := EvaluateBusinessReceipts(params, readOnly, []BusinessReceipt{}, review); !got.Accept || got.Result.Reason != "business_actions_not_requested" {
		t.Fatalf("unauthorized action was reviewed: %+v", got)
	}
}

func TestUnattemptedRequiredCapabilitiesIgnoreAnyRecordedOperation(t *testing.T) {
	other := "forge:action:record.other"
	params := BusinessReceiptParameters{RequiredCapabilityIDs: []string{other, receiptAction}, WhenAuthorized: true}
	scope := receiptScope()
	scope.AllowedCapabilityIDs = []string{receiptAction, other}
	if got := UnattemptedRequiredCapabilities(params, scope, []BusinessReceipt{}); strings.Join(got, ",") != "forge:action:record.other,forge:action:record.submit" {
		t.Fatalf("unattempted = %v", got)
	}
	for _, status := range []string{"succeeded", "failed", "unknown"} {
		if got := UnattemptedRequiredCapabilities(params, scope, []BusinessReceipt{receipt(status)}); strings.Join(got, ",") != other {
			t.Fatalf("%s operation still forced: %v", status, got)
		}
	}
	scope.AllowedCapabilityIDs = []string{receiptAction}
	if got := UnattemptedRequiredCapabilities(params, scope, []BusinessReceipt{}); strings.Join(got, ",") != receiptAction {
		t.Fatalf("unauthorized capability forced: %v", got)
	}
}
