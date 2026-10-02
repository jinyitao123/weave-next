package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/deliverycheck"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/workflow"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func completionFixture() (machine.GraphDefinition, deliverycheck.BusinessReceiptFrame) {
	c := &deliverable.DeliveryContract{Version: 1, ExternalEffects: deliverable.ExternalEffectsRequired, ExternalEffectsCheckID: "effects", RequiredChecks: []deliverable.CheckSpec{{ID: "effects", VerifierID: deliverycheck.BusinessReceiptsID, VerifierVersion: "v1", Parameters: json.RawMessage(`{"required_capability_ids":["forge:action:record.submit"],"when_authorized":true,"allow_needs_input":true}`)}}}
	return machine.GraphDefinition{DeliveryContract: c}, deliverycheck.BusinessReceiptFrame{Contract: c, Scope: deliverycheck.BusinessReceiptScope{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot", InputRevisionID: "input", SubjectID: "employee", AllowedCapabilityIDs: []string{"forge:action:record.submit"}, ObjectName: "record", RecordID: "record-1"}, Receipts: []deliverycheck.BusinessReceipt{}}
}
func TestUnconfiguredCompletionNeverReadsReceiptStore(t *testing.T) {
	r := &WorkflowSerialRuntime{BusinessReceiptReader: func(context.Context, deliverable.Candidate) (deliverycheck.BusinessReceiptFrame, error) {
		t.Fatal("unconfigured flow read new receipt store")
		return deliverycheck.BusinessReceiptFrame{}, errors.New("unavailable")
	}}
	if _, err := r.completionContext(TeamRun{}, machine.GraphDefinition{})(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredCompletionRequiresFrozenCheck(t *testing.T) {
	g, frame := completionFixture()
	frame.Contract = nil
	r := &WorkflowSerialRuntime{BusinessReceiptReader: func(context.Context, deliverable.Candidate) (deliverycheck.BusinessReceiptFrame, error) {
		return frame, nil
	}}
	_, err := r.completionContext(TeamRun{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot"}, g)(t.Context())
	if err == nil || !isCompletionCheckError(err) || ClassifyFailure(err).Retryable {
		t.Fatalf("missing frozen declaration did not hard stop: %v", err)
	}
}

func TestCompletionReadsFreshReceiptsAndHardStopsWithoutAnotherModelCall(t *testing.T) {
	for _, status := range []string{"failed", "unknown", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			g, frame := completionFixture()
			reads := 0
			r := &WorkflowSerialRuntime{BusinessReceiptReader: func(context.Context, deliverable.Candidate) (deliverycheck.BusinessReceiptFrame, error) {
				reads++
				if reads > 1 {
					if status == "unavailable" {
						return frame, errors.New("private database failure")
					}
					frame.Receipts = []deliverycheck.BusinessReceipt{{WorkspaceID: "ws", RunID: "run", InputRevisionID: "input", CapabilityID: "forge:action:record.submit", ObjectName: "record", RecordID: "record-1", OperationID: "effect-1", Status: status}}
				}
				return frame, nil
			}}
			ctx, err := r.completionContext(TeamRun{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "snapshot"}, g)(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			llm := &nodeContractLLM{responses: []string{`{"disposition":"complete","summary":"done","missing_items":[]}`}}
			compiled, err := compiler.CompileAgent("ws", &registry.AgentRecord{Name: "writer", Model: "test", Compaction: &registry.CompactionConfig{Enabled: false}}, llm, &compiler.CompileGuardDispatcher{}, compiler.CompileOpts{})
			if err != nil {
				t.Fatal(err)
			}
			node := machine.Node{ID: "final", Type: machine.NodeWorker, Config: machine.WorkerConfig{AgentID: "writer", AgentVersion: 1}, Output: &machine.OutputContract{Type: machine.ValueJSON, Schema: machine.WorkbenchResultSchemaV1()}}
			_, _, err = runAgentNode(ctx, node, frozen.ArtifactPayloadV1{}, map[string]workflow.RuntimeGraphEntry{runtimeEntryKey("writer", 1): {AgentID: "writer", AgentVersion: 1, Graph: compiled}}, "input", nil, nil, "", nil, nil, true)
			if err == nil || len(llm.requests) != 1 || reads != 2 {
				t.Fatalf("err=%v requests=%d reads=%d", err, len(llm.requests), reads)
			}
			failure := ClassifyFailure(err)
			if failure.Class != FailureClassVerification || failure.Retryable {
				t.Fatalf("completion hardstop became retryable: %+v", failure)
			}
		})
	}
}
