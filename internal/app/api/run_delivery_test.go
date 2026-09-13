package api

import (
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
)

func TestRunDeliveryProjectsReadableMechanicalMismatch(t *testing.T) {
	state := deliverable.DeliveryState{RevisionID: "revision", ContractDigest: "contract", VerificationID: "report", Report: &deliverable.VerificationReport{
		ID: "report", RevisionID: "revision", ContractDigest: "contract", Status: deliverable.VerificationFailed,
		Checks: []deliverable.CheckResult{{CheckID: "total", Title: "总额应等于原始明细合计", VerifierID: "weave.deterministic", VerifierVersion: "v1", Status: deliverable.VerificationFailed, Reason: "deterministic_mismatch", Evidence: json.RawMessage(`{"actual":"294","expected":"295","input_digest":"private-evidence"}`)}},
	}}
	view := projectRunDelivery(teamrun.StatusSucceeded, state)
	if len(view.Checks) != 1 || view.Checks[0].Title != "总额应等于原始明细合计" || view.Checks[0].Actual != "294" || view.Checks[0].Expected != "295" || view.VerificationStatus != deliverable.VerificationFailed {
		t.Fatalf("failure details lost: %+v", view)
	}
}

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
