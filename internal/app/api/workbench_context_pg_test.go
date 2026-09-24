package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
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
	if err := server.Tasks.CompleteClaimed(t.Context(), claimed.ID, "context-worker", json.RawMessage(`{"ok":true}`), run.RunID); err != nil {
		t.Fatal(err)
	}
	finalContent := "最终合同核对结果"
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_final_deliverables
		(id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
		VALUES($1,'ws','user-a','lead',$2,$3,$4,$4,'核对结果',$5,'text/plain','{"artifact_kind":"final"}'::jsonb)`,
		"deliverable-context-a", registration.WorkbenchSessionID, uuid.NewString(), run.RunID, finalContent); err != nil {
		t.Fatal(err)
	}

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
	if response.Input.Parent == nil || response.Input.Parent.RootInputRevisionID != input.InputRevisionID || response.Run.Status != "succeeded" ||
		response.Run.FinalResult == nil || response.Run.FinalResult.Content != finalContent ||
		response.Run.FinalResult.SHA256 != dispatchInputDigest([]byte(finalContent)) {
		t.Fatalf("unexpected lineage or run result: input=%+v run=%+v", response.Input, response.Run)
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
