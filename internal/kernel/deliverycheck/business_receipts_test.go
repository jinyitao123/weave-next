package deliverycheck

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

const receiptAction = "forge:action:record.submit"

func receiptScope() BusinessReceiptScope {
	return BusinessReceiptScope{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", InputRevisionID: "input", SubjectID: "employee", AllowedCapabilityIDs: []string{receiptAction}, ObjectName: "record", RecordID: "record-1"}
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
