package api

import (
	"encoding/json"
	"net/http"
	"reflect"
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

func TestRevisionDispatchRetainsOriginalMaterialsAndContractRealPG(t *testing.T) {
	graph := json.RawMessage(`{"schema_version":1,"entry_node_id":"deliver","input_contract":{"type":"text"},"output_contract":{"type":"text"},"delivery_contract":{"version":1,"coverage":"explicit","output":{"type":"text"},"required_artifacts":[{"id":"report","path":"outputs/report.md"}],"external_effects":"none"},"nodes":[{"id":"deliver","type":"deliver","config":{"result":{"source":"run_input","path":""}}}],"edges":[]}`)
	server, pool := newTeamDispatchTestServerWithGraph(t, graph)
	original := dispatchInputRegistrationFixture("revision-session", "整理原始材料并计算应付总额。", "")
	registered, err := registerInputForTest(server, original)
	if err != nil || registered.Code != http.StatusCreated {
		t.Fatalf("register original: %s %v", registered.Body.String(), err)
	}
	var first dispatchInputReceipt
	if err := json.Unmarshal(registered.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	dispatched, err := boundDispatchForTest(server, map[string]any{"input_revision_id": first.InputRevisionID}, "user")
	if err != nil || dispatched.Code != http.StatusCreated {
		t.Fatalf("dispatch original: %s %v", dispatched.Body.String(), err)
	}
	var firstRun workflowManualRunResponse
	if err := json.Unmarshal(dispatched.Body.Bytes(), &firstRun); err != nil {
		t.Fatal(err)
	}
	priorContent := "# 上一版报告\n应付总额：300 元（待复核）"
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_final_deliverables
		(id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
		VALUES('prior-report','ws','user','lead','revision-session','prior-event',$1,$1,'上一版报告',$2,'text/markdown','{"artifact_kind":"final"}')`,
		firstRun.RunID, priorContent); err != nil {
		t.Fatal(err)
	}

	change := "请按原始材料复核总额，修正上一版的计算错误。"
	revision := dispatchInputRegistrationFixture("revision-session", change, first.InputRevisionID)
	revision.RevisionContext = &dispatchRevisionContext{ParentInputRevisionID: first.InputRevisionID, ParentRunID: firstRun.RunID}
	revisedResponse, err := registerInputForTest(server, revision)
	if err != nil || revisedResponse.Code != http.StatusCreated {
		t.Fatalf("register revision: %s %v", revisedResponse.Body.String(), err)
	}
	var revised dispatchInputReceipt
	if err := json.Unmarshal(revisedResponse.Body.Bytes(), &revised); err != nil {
		t.Fatal(err)
	}
	stored, err := server.loadDispatchInput(t.Context(), "ws", "user", revised.InputRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Task != change || stored.RevisionKind != "revision" || stored.RootInputRevisionID != first.InputRevisionID ||
		stored.ParentInputRevisionID != first.InputRevisionID || stored.ParentRunID != firstRun.RunID ||
		!strings.Contains(stored.ExecutionTask, original.Task) || !strings.Contains(stored.ExecutionTask, priorContent) ||
		!strings.Contains(stored.ExecutionTask, change) || stored.ParentDeliveryDigest == "" {
		t.Fatalf("revision lineage or materials missing: %+v", stored)
	}
	var inherited, firstContract map[string]any
	if json.Unmarshal(stored.DeliveryContract, &inherited) != nil {
		t.Fatal("revision contract unreadable")
	}
	firstStored, err := server.loadDispatchInput(t.Context(), "ws", "user", first.InputRevisionID)
	if err != nil || json.Unmarshal(firstStored.DeliveryContract, &firstContract) != nil || !reflect.DeepEqual(inherited, firstContract) {
		t.Fatalf("revision did not inherit business contract: revised=%s original=%s err=%v", stored.DeliveryContract, firstStored.DeliveryContract, err)
	}
	revisedDispatch, err := boundDispatchForTest(server, map[string]any{"input_revision_id": revised.InputRevisionID}, "user")
	if err != nil || revisedDispatch.Code != http.StatusCreated {
		t.Fatalf("dispatch revision: %s %v", revisedDispatch.Body.String(), err)
	}
	var revisedRun workflowManualRunResponse
	if err := json.Unmarshal(revisedDispatch.Body.Bytes(), &revisedRun); err != nil {
		t.Fatal(err)
	}
	queued, err := server.Tasks.Get(t.Context(), "ws", revisedRun.TaskID)
	var queuedTask string
	if err != nil || json.Unmarshal(queued.Payload, &queuedTask) != nil || queuedTask != stored.ExecutionTask {
		t.Fatalf("revision queue lost server materialization: task=%q err=%v", queuedTask, err)
	}
}
