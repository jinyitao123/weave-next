package deliveryverify

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
)

func TestBusinessReceiptDeliveryVerifierUsesTrustedFrozenFrame(t *testing.T) {
	check := deliverable.CheckSpec{ID: "effects", VerifierID: deliverycheck.BusinessReceiptsID, VerifierVersion: "v1", Parameters: json.RawMessage(`{"required_capability_ids":["forge:action:record.submit"],"when_authorized":true,"allow_needs_input":true}`)}
	frame := deliverycheck.BusinessReceiptFrame{Contract: &deliverable.DeliveryContract{ExternalEffectsCheckID: "effects", RequiredChecks: []deliverable.CheckSpec{check}}, Scope: deliverycheck.BusinessReceiptScope{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", InputRevisionID: "input", SubjectID: "employee", AllowedCapabilityIDs: []string{"forge:action:record.submit"}, ObjectName: "record", RecordID: "record-1"}, Receipts: []deliverycheck.BusinessReceipt{}}
	input := deliverable.VerificationInput{Check: check, Candidate: deliverable.Candidate{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", Output: json.RawMessage(`{"disposition":"complete","summary":"done","missing_items":[]}`)}}
	verify := businessReceiptVerifier(func(context.Context, deliverable.Candidate) (deliverycheck.BusinessReceiptFrame, error) {
		return frame, nil
	})
	result, err := verify(t.Context(), input)
	if err != nil || result.Status != deliverable.VerificationFailed || result.Reason != "required_business_action_missing" {
		t.Fatalf("missing call=%+v %v", result, err)
	}
	frame.Receipts = []deliverycheck.BusinessReceipt{{WorkspaceID: "ws", RunID: "run", InputRevisionID: "input", CapabilityID: "forge:action:record.submit", ObjectName: "record", RecordID: "record-1", OperationID: "stable-operation", Status: "succeeded"}}
	result, err = verify(t.Context(), input)
	if err != nil || result.Status != deliverable.VerificationPassed {
		t.Fatalf("real receipt=%+v %v", result, err)
	}
	frame.Scope.RunID = "other"
	if _, err = verify(t.Context(), input); err == nil {
		t.Fatal("reader crossed run boundary")
	}
	frame.Scope.RunID = "run"
	input.Candidate.Output = json.RawMessage(`{"disposition":"needs_input","summary":"missing","missing_items":[""]}`)
	if _, err = verify(t.Context(), input); err == nil {
		t.Fatal("invalid protocol bypassed completion check")
	}
	verify = businessReceiptVerifier(func(context.Context, deliverable.Candidate) (deliverycheck.BusinessReceiptFrame, error) {
		return frame, errors.New("read unavailable")
	})
	if _, err = verify(t.Context(), input); err == nil {
		t.Fatal("read failure became empty outcomes")
	}
}
