package api

import (
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/teamrun"
)

func TestRunDeliveryNeverInfersVerificationFromExecution(t *testing.T) {
	state := deliverable.DeliveryState{ContractDigest: "contract"}
	for _, status := range []teamrun.Status{teamrun.StatusSucceeded, teamrun.StatusFailed, teamrun.StatusCancelled} {
		if got := projectRunDelivery(status, state); got.VerificationStatus != deliverable.VerificationUnknown {
			t.Fatalf("terminal execution %s without report became %s", status, got.VerificationStatus)
		}
	}
	if got := projectRunDelivery(teamrun.StatusRunning, state); got.VerificationStatus != deliverable.VerificationPending {
		t.Fatalf("before deliver boundary = %s", got.VerificationStatus)
	}
	for _, status := range []deliverable.VerificationStatus{deliverable.VerificationPassed, deliverable.VerificationFailed, deliverable.VerificationUnknown} {
		state.RevisionID, state.VerificationID = "delivery-v1", "report-v1"
		state.Report = &deliverable.VerificationReport{ID: "report-v1", RevisionID: "delivery-v1", ContractDigest: "contract", Status: status}
		got := projectRunDelivery(teamrun.StatusSucceeded, state)
		if got.VerificationStatus != status || got.RevisionID != "delivery-v1" {
			t.Fatalf("explicit report status lost: %+v", got)
		}
		state.RevisionID = "delivery-v2"
		if got := projectRunDelivery(teamrun.StatusSucceeded, state); got.VerificationStatus != deliverable.VerificationUnknown || got.Reason != "delivery_report_mismatch" {
			t.Fatalf("old report accepted for new delivery: %+v", got)
		}
	}
}
