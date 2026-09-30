package publicationservice

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/publication"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

func TestRunDeliveryContractDefaultsOverridesAndKeepsTheGraphOutput(t *testing.T) {
	graph := machine.GraphDefinition{}
	graph.OutputContract.Type = "text"
	contract, err := runDeliveryContract(graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	if contract.Coverage != "incomplete" || contract.Output.Type != "text" {
		t.Fatalf("default contract = %+v", contract)
	}
	own := &deliverable.DeliveryContract{Version: 1, Coverage: "incomplete", Limitations: []string{"graph-declared"}}
	graph.DeliveryContract = own
	fromGraph, err := runDeliveryContract(graph, nil)
	if err != nil || len(fromGraph.Limitations) != 1 || fromGraph.Limitations[0] != "graph-declared" || fromGraph.Output.Type != "text" {
		t.Fatalf("graph contract = %+v err=%v", fromGraph, err)
	}
	requested := &deliverable.DeliveryContract{Version: 1, Coverage: "incomplete", Limitations: []string{"caller-declared"}}
	fromCaller, err := runDeliveryContract(graph, requested)
	if err != nil || fromCaller.Limitations[0] != "caller-declared" {
		t.Fatalf("caller contract = %+v err=%v", fromCaller, err)
	}
	if own.Output.Type != "" || requested.Output.Type != "" {
		t.Fatal("deriving the run contract mutated a caller's contract")
	}
}

// A developer trial must meet the delivery contract the published team will,
// so a trial that passes is evidence about the real run.
func TestCandidateAdmissionFreezesTheDeliveryContractRealPG(t *testing.T) {
	ctx, service, pool, publish := publicationPGFixture(t)
	deadline := time.Now().UTC().Add(time.Hour)
	request := publication.CandidateRunRequest{Version: publication.ContractVersion, RequestID: "candidate-contract", Candidate: publish.Candidate,
		Input: json.RawMessage(`{"input":"test"}`), InputVersion: "input-contract", SourceRef: "product-request", Purpose: "verification", DeadlineAt: &deadline}
	receipt, err := service.AdmitCandidate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	var published, revision string
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT published_digest,input_revision_id,contract FROM weave_run_delivery_state WHERE workspace_id='workspace' AND run_snapshot_id=$1`,
		receipt.RunSnapshotID).Scan(&published, &revision, &raw); err != nil {
		t.Fatalf("a candidate run admitted without a frozen delivery contract: %v", err)
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(request.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	graph, report := machine.DecodeGraphDefinitionV1(payload.GraphDefinition)
	if report != nil && len(report.Issues) > 0 {
		t.Fatal("fixture graph is invalid")
	}
	want, err := runDeliveryContract(graph, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored deliverable.DeliveryContract
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if published != request.Candidate.ContentHash || revision != request.InputVersion || stored.Output.Type != want.Output.Type || string(stored.Output.Schema) != string(want.Output.Schema) || stored.Coverage != want.Coverage {
		t.Fatalf("frozen contract %+v differs from what a published run derives %+v (digest %q, input %q)", stored, want, published, revision)
	}
	replayed, err := service.AdmitCandidate(ctx, request)
	if err != nil || replayed != receipt {
		t.Fatalf("admission replay changed after freezing a contract: %v", err)
	}
}
