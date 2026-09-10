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

func TestPublishedDeliveryContractDefaultsNaturalWorkbenchDispatchRealPG(t *testing.T) {
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"text"},"output_contract":{"type":"text"},"delivery_contract":{"version":1,"coverage":"explicit","output":{"type":"text"},"required_artifacts":[{"id":"page","path":"outputs/index.html"}],"external_effects":"none"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`)
	server, _ := newTeamDispatchTestServerWithGraph(t, graph)
	task := "请按已确认的团队范围完成本地页面，并整理好可以打开的结果。"
	registration := dispatchInputRegistrationFixture("natural-contract-session", task, "")
	registered, err := registerInputForTest(server, registration)
	if err != nil || registered.Code != http.StatusCreated {
		t.Fatalf("registration: %s %v", registered.Body.String(), err)
	}
	var receipt dispatchInputReceipt
	if err := json.Unmarshal(registered.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	dispatched, err := boundDispatchForTest(server, map[string]any{"input_revision_id": receipt.InputRevisionID, "client_request_id": receipt.ClientRequestID}, "user")
	if err != nil || dispatched.Code != http.StatusCreated {
		t.Fatalf("dispatch: %s %v", dispatched.Body.String(), err)
	}
	var run workflowManualRunResponse
	if err := json.Unmarshal(dispatched.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	state, err := server.Deliverables.GetDeliveryState(t.Context(), "ws", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	contract := state.Binding.Contract
	if contract == nil || contract.Coverage != "explicit" || len(contract.RequiredArtifacts) != 1 || contract.RequiredArtifacts[0].Path != "outputs/index.html" || contract.ExternalEffects != "none" || contract.Output.Type != "text" {
		t.Fatalf("published contract was not frozen for natural dispatch: %+v", contract)
	}

	overrideTask := "请改为交付报告。\n```weave-delivery-contract-v1\n" + `{"version":1,"coverage":"explicit","required_artifacts":[{"id":"report","path":"outputs/report.md"}],"external_effects":"none"}` + "\n```"
	overrideRegistration := dispatchInputRegistrationFixture("override-contract-session", overrideTask, "")
	overrideRegistered, err := registerInputForTest(server, overrideRegistration)
	if err != nil || overrideRegistered.Code != http.StatusCreated {
		t.Fatalf("override registration: %s %v", overrideRegistered.Body.String(), err)
	}
	if err := json.Unmarshal(overrideRegistered.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	overrideDispatched, err := boundDispatchForTest(server, map[string]any{"input_revision_id": receipt.InputRevisionID, "client_request_id": receipt.ClientRequestID}, "user")
	if err != nil || overrideDispatched.Code != http.StatusCreated {
		t.Fatalf("override dispatch: %s %v", overrideDispatched.Body.String(), err)
	}
	if err := json.Unmarshal(overrideDispatched.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	overrideState, err := server.Deliverables.GetDeliveryState(t.Context(), "ws", run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	override := overrideState.Binding.Contract
	if override == nil || len(override.RequiredArtifacts) != 1 || override.RequiredArtifacts[0].Path != "outputs/report.md" {
		t.Fatalf("exact user contract did not override published default: %+v", override)
	}
}
