package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
)

type verifiedTrialFallback struct {
	outputs []deliverable.WorkflowOutput
	fence   deliverable.VerificationFence
	err     error
}

func (*verifiedTrialFallback) RecordWorkflowOutput(context.Context, deliverable.WorkflowOutput) error {
	return errors.New("must not downgrade verified delivery to ordinary recording")
}

func (f *verifiedTrialFallback) RecordVerifiedWorkflowOutputs(_ context.Context, outputs []deliverable.WorkflowOutput, fence deliverable.VerificationFence) (deliverable.VerificationReport, error) {
	f.outputs, f.fence = outputs, fence
	return deliverable.VerificationReport{Status: deliverable.VerificationPassed}, f.err
}

func TestDevelopmentRecorderPreservesVerifiedDeliveryAndResultClassification(t *testing.T) {
	fallback := &verifiedTrialFallback{}
	recorder := &developmentTrialWorkflowOutputRecorder{fallback: fallback}
	outputs := []deliverable.WorkflowOutput{{Final: true, NodeID: "deliver", Output: map[string]any{"disposition": "needs_input"},
		ResultMetadata: json.RawMessage(`{"protocol":"workbench_result_v1","disposition":"needs_input","summary":"缺少指定附件","missing_items":["技术协议"]}`)}}
	fence := deliverable.VerificationFence{WorkspaceID: "workspace", RunID: "run", ExecutorID: "executor"}
	report, err := recorder.RecordVerifiedWorkflowOutputs(t.Context(), outputs, fence)
	if err != nil || report.Status != deliverable.VerificationPassed || !reflect.DeepEqual(fallback.outputs, outputs) || fallback.fence != fence {
		t.Fatalf("verified delivery was not preserved: report=%+v err=%v", report, err)
	}
	fallback.err = deliverable.ErrVerificationFence
	report, err = recorder.RecordVerifiedWorkflowOutputs(t.Context(), outputs, fence)
	if !errors.Is(err, deliverable.ErrVerificationFence) || report.Status != "" {
		t.Fatalf("verification rejection must not become success: report=%+v err=%v", report, err)
	}
}
