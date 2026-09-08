package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDispatchDeliveryContractRequiresExactUserStructure(t *testing.T) {
	valid := "```weave-delivery-contract-v1\n" + `{"version":1,"coverage":"explicit","required_artifacts":[{"id":"report","path":"report.txt","contains":["INV-440"]}],"required_checks":[{"id":"effects","verifier_id":"fixture.effects","verifier_version":"v1"}],"external_effects_check_id":"effects"}` + "\n```"
	contract, err := parseDispatchDeliveryContract("用户完整原话\n" + valid + "\n确认派发")
	if err != nil || contract == nil || contract.RequiredArtifacts[0].Path != "report.txt" || contract.RequiredArtifacts[0].Contains[0] != "INV-440" {
		t.Fatalf("exact user contract not preserved: contract=%+v err=%v", contract, err)
	}
	for _, task := range []string{"请生成报告，Reviewer 必须 PASS", `{"version":1,"coverage":"explicit"}`, "```json\n{\"version\":1}\n```"} {
		if contract, err := parseDispatchDeliveryContract(task); err != nil || contract != nil {
			t.Fatalf("free text became explicit scope: %q %+v %v", task, contract, err)
		}
	}
	for _, task := range []string{
		valid + "\n" + valid,
		strings.TrimSuffix(valid, "```"),
		strings.Replace(valid, `"version":1`, `"version":1,"version":2`, 1),
		strings.Replace(valid, `"version":1`, `"version":1,"output":{"type":"text"}`, 1),
		strings.Replace(valid, `"version":1`, `"version":1,"verification_status":"passed"`, 1),
		strings.Replace(valid, `"report.txt"`, `"../report.txt"`, 1),
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
	} {
		if _, err := parseDispatchDeliveryContract(task); err == nil {
			t.Fatalf("ambiguous or unauthorized contract accepted: %s", task)
		}
	}
}

func TestDispatchDeliveryContractFreezesWithAdmissionRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	ctx := t.Context()
	task := "原始 INV-440 任务\n```weave-delivery-contract-v1\n" + `{"version":1,"coverage":"explicit","required_artifacts":[{"id":"report","path":"report.txt"}]}` + "\n```\n"
	registration := dispatchInputRegistrationFixture("contract-session", task, "")
	registered, err := registerInputForTest(server, registration)
	if err != nil || registered.Code != http.StatusCreated {
		t.Fatalf("registration: %s %v", registered.Body.String(), err)
	}
	var receipt dispatchInputReceipt
	if err := json.Unmarshal(registered.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE weave_dispatch_input_revisions SET delivery_contract='{}'::jsonb WHERE input_revision_id=$1`,
		`UPDATE weave_dispatch_input_revisions SET task='replaced' WHERE input_revision_id=$1`,
	} {
		if _, err := pool.Exec(ctx, sql, receipt.InputRevisionID); err == nil {
			t.Fatal("registered source contract was mutable")
		}
	}
	dispatched, err := boundDispatchForTest(server, map[string]any{"input_revision_id": receipt.InputRevisionID, "client_request_id": receipt.ClientRequestID}, "user")
	if err != nil || dispatched.Code != http.StatusCreated {
		t.Fatalf("dispatch: %s %v", dispatched.Body.String(), err)
	}
	var run workflowManualRunResponse
	if err := json.Unmarshal(dispatched.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	state, err := server.Deliverables.GetDeliveryState(ctx, "ws", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Binding.InputRevisionID != receipt.InputRevisionID || state.Binding.RunSnapshotID != run.RunID || state.Binding.WorkflowVersion != 1 || state.Binding.Contract == nil || state.Binding.Contract.RequiredArtifacts[0].Path != "report.txt" || state.Binding.Contract.Output.Type == "" || state.Binding.PublishedDigest == "" {
		t.Fatalf("incomplete admission binding: %+v", state.Binding)
	}
	if state.VerificationID != "" || state.RevisionID != "" {
		t.Fatal("dispatch invented a delivery report")
	}
	stored, err := server.Tasks.Get(ctx, "ws", run.TaskID)
	var actual string
	if err != nil || json.Unmarshal(stored.Payload, &actual) != nil || actual != task {
		t.Fatal("contract extraction changed original task bytes")
	}
	replay, err := boundDispatchForTest(server, map[string]any{"input_revision_id": receipt.InputRevisionID, "client_request_id": receipt.ClientRequestID}, "user")
	if err != nil || replay.Code != http.StatusOK {
		t.Fatalf("replay: %s %v", replay.Body.String(), err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM weave_run_delivery_state`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry created new contract: count=%d err=%v", count, err)
	}
}
