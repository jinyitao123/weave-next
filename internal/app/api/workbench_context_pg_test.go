package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

func callWorkbenchRunContext(t *testing.T, server *Server, userID, runID string) (int, workbenchContextResponse) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/workbench-context", nil)
	c := echo.New().NewContext(request, recorder)
	c.Set("tenant", "ws")
	c.Set("user_id", userID)
	c.SetParamNames("id")
	c.SetParamValues(runID)
	if err := server.handleGetWorkbenchRunContext(c); err != nil {
		t.Fatal(err)
	}
	var response workbenchContextResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return recorder.Code, response
}

func TestWorkbenchContextReadsExactInputAndRejectsOtherEmployeesRealPG(t *testing.T) {
	t.Setenv("WEAVE_SECRET_KEY_FILE", "")
	t.Setenv("WEAVE_SECRET_KEY", strings.Repeat("22", 32))
	server, pool := newTeamDispatchTestServer(t)
	fileContent := []byte("fixed Forge material")
	fileSHA := dispatchInputDigest(fileContent)
	forge := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/storage/files/file-a" || request.Header.Get("Authorization") != "Bearer fixture-token" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = writer.Write(fileContent)
	}))
	defer forge.Close()
	server.ExternalIdentity = externalIdentityVerifierFunc(func(_ context.Context, token string) (ExternalIdentity, error) {
		if token != "fixture-token" {
			return ExternalIdentity{}, errors.New("unexpected fixture token")
		}
		return ExternalIdentity{Issuer: forge.URL, Subject: "forge-user", Organization: "ws"}, nil
	})
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id)
		VALUES($1,'forge-user','ws','user-a')`, forge.URL); err != nil {
		t.Fatal(err)
	}

	registration := dispatchInputRegistrationFixture("workbench-session-a", "核对这份固定文件", "")
	registration.SourceMessages[0].SHA256 = strings.ToUpper(registration.SourceMessages[0].SHA256)
	version := 1
	registration.WorkflowID, registration.WorkflowVersion = "flow", &version
	registration.ProjectID = workbenchProjectID("user-a")
	emptyActions := []string{}
	registration.AuthorizedBusinessCapabilityIDs = &emptyActions
	registration.Resources = []dispatchInputResource{{Type: "forge-file", ID: "file-a", Name: "材料.txt", Bytes: int64(len(fileContent)), SHA256: fileSHA}}
	registration.BusinessRecord = &dispatchBusinessRecord{ObjectName: "sales_contract", RecordID: "record-a"}
	body, err := json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/workbench/dispatch-inputs", strings.NewReader(string(body)))
	request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user-a"}))
	recorder := httptest.NewRecorder()
	c := echo.New().NewContext(request, recorder)
	c.Set("tenant", "ws")
	c.Set("user_id", "user-a")
	c.Request().Header.Set(forgeDelegationHeader, "Bearer fixture-token")
	if err := server.handleRegisterDispatchInput(c); err != nil || recorder.Code != http.StatusCreated {
		t.Fatalf("register input: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
	var input dispatchInputReceipt
	if err := json.Unmarshal(recorder.Body.Bytes(), &input); err != nil {
		t.Fatal(err)
	}
	dispatched, err := boundDispatchForTest(server, map[string]any{"input_revision_id": input.InputRevisionID, "client_request_id": input.ClientRequestID}, "user-a")
	if err != nil || dispatched.Code != http.StatusCreated {
		t.Fatalf("dispatch input: status=%d body=%s err=%v", dispatched.Code, dispatched.Body.String(), err)
	}
	var run workflowManualRunResponse
	if err := json.Unmarshal(dispatched.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}

	claimed, err := server.Tasks.Claim(t.Context(), "context-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: "ws", IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || claimed == nil || claimed.ID != run.TaskID {
		t.Fatalf("claim run task: task=%+v err=%v", claimed, err)
	}
	consumer := &teamrun.Consumer{
		Transactions: pool,
		Snapshots:    server.Snapshots,
		Runs:         teamrun.NewPGStore(),
		Tasks:        server.Tasks,
	}
	if established, err := consumer.ConsumeClaimed(t.Context(), claimed, "context-worker"); err != nil || established.RunID != run.RunID {
		t.Fatalf("establish team run before recording activity: run=%+v err=%v", established, err)
	}
	actionStore := &teamrun.PGActivityStore{Transactions: pool}
	writeActionEvent := func(nodeID, memberID, phase, callID, actionName, actionLabel, objectName, recordID, status string) {
		t.Helper()
		kind := "business_action_started"
		if phase == "result" {
			kind = "business_action_result"
		}
		detail, err := json.Marshal(map[string]any{
			"source": "forge_mcp.run_action", "phase": phase, "invocation_id": "snapshot/0/" + nodeID,
			"tool_call_id": callID, "capability_id": "forge:action:" + objectName + "." + actionName,
			"action_key": objectName + "." + actionName, "action_name": actionName, "action_label": actionLabel,
			"object_name": objectName, "input_revision_id": input.InputRevisionID,
			"record_id": recordID, "status": status,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := actionStore.RecordBusinessActionEvent(t.Context(), teamrun.ActivityEvent{
			WorkspaceID: "ws", RunID: run.RunID, EventID: uuid.NewString(), Kind: kind,
			NodeID: nodeID, MemberID: memberID, MemberVersion: 1,
			Detail: detail, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeActionEvent("lead", "lead-agent", "started", "forge-call-1", "ContractSubmit", "提交指定合同版本", "sales_contract", "record-a", "")
	writeActionEvent("lead", "lead-agent", "result", "forge-call-1", "ContractSubmit", "提交指定合同版本", "sales_contract", "record-a", "succeeded")
	writeActionEvent("review", "review-agent", "started", "forge-call-2", "RequestRevision", "要求修订", "sales_contract", "record-a", "")
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_team_run_activity_events
		(workspace_id,run_id,event_id,kind,node_id,member_id,member_version,detail,occurred_at)
		VALUES('ws',$1,$2,'member_completed','lead','lead-agent',1,$3::jsonb,statement_timestamp())`,
		run.RunID, uuid.NewString(), `{"summary":"另一个成员声称已经执行了审批动作"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_team_runs SET status='failed',error_code='team_run_execution_failed',cause_summary='later member failed',terminal_at=statement_timestamp()
		WHERE workspace_id='ws' AND run_id=$1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	finalContent := `{"disposition":"needs_input","summary":"缺少原始签署日期","missing_items":["提供完整签署日期"]}`
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_final_deliverables
		(id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
		VALUES($1,'ws','user-a','lead',$2,$3,$4,$4,'核对结果',$5,'application/json',
		'{"artifact_kind":"final","workbench_result":{"protocol":"workbench_result_v1","disposition":"needs_input","summary":"缺少原始签署日期","missing_items":["提供完整签署日期"]}}'::jsonb)`,
		"deliverable-context-a", registration.WorkbenchSessionID, uuid.NewString(), run.RunID, finalContent); err != nil {
		t.Fatal(err)
	}

	// Recreate the read store to prove that receipts survive a server restart.
	server.teamRunActivities = &teamrun.PGActivityStore{Transactions: pool}
	status, response := callWorkbenchRunContext(t, server, "user-a", run.RunID)
	if status != http.StatusOK || response.Version != "1" || response.Source.InputRevisionID != input.InputRevisionID ||
		response.Source.RunID != run.RunID || response.Source.WorkbenchSessionID != registration.WorkbenchSessionID {
		t.Fatalf("unexpected continuation source: status=%d response=%+v", status, response)
	}
	if response.Input.Task != registration.Task || response.Input.TaskSHA256 != input.TaskSHA256 || response.Input.TeamID != "team" ||
		response.Input.WorkflowID != "flow" || response.Input.WorkflowVersion != 1 || len(response.Input.SourceMessages) != 1 ||
		response.Input.SourceMessages[0].MessageID != registration.SourceMessages[0].MessageID ||
		response.Input.SourceMessages[0].SHA256 != registration.SourceMessages[0].SHA256 {
		t.Fatalf("unexpected fixed input: %+v", response.Input)
	}
	if len(response.Input.Materials) != 1 || response.Input.Materials[0].ID != "file-a" ||
		response.Input.Materials[0].SHA256 != fileSHA || response.Input.BusinessRecord == nil ||
		response.Input.BusinessRecord.ObjectName != "sales_contract" || response.Input.BusinessRecord.RecordID != "record-a" {
		t.Fatalf("delegated resources were not projected exactly: %+v", response.Input)
	}
	if response.Input.Parent == nil || response.Input.Parent.RootInputRevisionID != input.InputRevisionID || response.Run.Status != "failed" ||
		response.Run.FinalResult == nil || response.Run.FinalResult.Content != finalContent ||
		response.Run.FinalResult.SHA256 != dispatchInputDigest([]byte(finalContent)) ||
		response.Run.FinalResult.Disposition != "needs_input" || response.Run.FinalResult.Summary != "缺少原始签署日期" ||
		response.Run.FinalResult.MissingItems == nil || len(*response.Run.FinalResult.MissingItems) != 1 ||
		(*response.Run.FinalResult.MissingItems)[0] != "提供完整签署日期" {
		t.Fatalf("unexpected lineage or run result: input=%+v run=%+v", response.Input, response.Run)
	}
	if len(response.Run.ActionOutcomes) != 2 {
		t.Fatalf("expected only the two platform-recorded business actions: %+v", response.Run.ActionOutcomes)
	}
	submitted, unknown := response.Run.ActionOutcomes[0], response.Run.ActionOutcomes[1]
	if submitted.NodeID != "lead" || submitted.CallID != "forge-call-1" || submitted.ActionName != "提交指定合同版本" ||
		submitted.ObjectName != "sales_contract" || submitted.RecordID != "record-a" || submitted.Status != "succeeded" ||
		!strings.Contains(submitted.Summary, "提交指定合同版本") || strings.Contains(submitted.Summary, "record-a") {
		t.Fatalf("unexpected confirmed action outcome: %+v", submitted)
	}
	if unknown.NodeID != "review" || unknown.CallID != "forge-call-2" || unknown.Status != "unknown" ||
		!strings.Contains(unknown.Summary, "结果未知") || strings.Contains(unknown.Summary, "sales_contract") || strings.Contains(unknown.Summary, "record-a") {
		t.Fatalf("unexpected unknown action outcome: %+v", unknown)
	}
	completeContent := `{"disposition":"complete","summary":"本轮检查已完成","missing_items":[]}`
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_final_deliverables
		(id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
		VALUES($1,'ws','user-a','lead',$2,$3,$4,$4,'完整检查意见',$5,'application/json',
		'{"artifact_kind":"final","workbench_result":{"protocol":"workbench_result_v1","disposition":"complete","summary":"本轮检查已完成","missing_items":[]}}'::jsonb)`,
		"deliverable-context-complete", registration.WorkbenchSessionID, uuid.NewString(), run.RunID, completeContent); err != nil {
		t.Fatal(err)
	}
	status, completeResponse := callWorkbenchRunContext(t, server, "user-a", run.RunID)
	if status != http.StatusOK || completeResponse.Run.FinalResult == nil || completeResponse.Run.FinalResult.Disposition != "complete" ||
		completeResponse.Run.FinalResult.Summary != "本轮检查已完成" || completeResponse.Run.FinalResult.MissingItems == nil ||
		len(*completeResponse.Run.FinalResult.MissingItems) != 0 {
		t.Fatalf("complete result lost its empty missing_items array: status=%d result=%+v", status, completeResponse.Run.FinalResult)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_final_deliverables
		(id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
		VALUES($1,'ws','user-a','lead',$2,$3,$4,$4,'错配检查意见',$5,'application/json',
		'{"artifact_kind":"final","workbench_result":{"protocol":"workbench_result_v1","disposition":"needs_input","summary":"不匹配的检查摘要","missing_items":["提供完整签署日期"]}}'::jsonb)`,
		"deliverable-context-mismatch", registration.WorkbenchSessionID, uuid.NewString(), run.RunID, finalContent); err != nil {
		t.Fatal(err)
	}
	if status, _ := callWorkbenchRunContext(t, server, "user-a", run.RunID); status != http.StatusServiceUnavailable {
		t.Fatalf("mismatched result metadata was projected: status=%d", status)
	}
	if status, _ := callWorkbenchRunContext(t, server, "user-b", run.RunID); status != http.StatusNotFound {
		t.Fatalf("another employee read this run context: status=%d", status)
	}
	if status, _ := callWorkbenchRunContext(t, server, "user-a", uuid.NewString()); status != http.StatusNotFound {
		t.Fatalf("missing run context was accepted: status=%d", status)
	}
	if status, _ := callWorkbenchRunContext(t, server, "", run.RunID); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated read was accepted: status=%d", status)
	}
}
