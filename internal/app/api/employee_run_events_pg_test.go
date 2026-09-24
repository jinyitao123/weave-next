package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

func TestEmployeeRunEventBackfillDeliversOnceToForgeInboxRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id)
		VALUES('https://forge.example.test','forge-user','ws','user')`); err != nil {
		t.Fatal(err)
	}
	registration := dispatchInputRegistrationFixture("workbench-session", "检查固定材料", "")
	created, err := registerInputForTest(server, registration)
	if err != nil || created.Code != http.StatusCreated {
		t.Fatalf("register input status=%d body=%s err=%v", created.Code, created.Body.String(), err)
	}
	var receipt dispatchInputReceipt
	if err := json.Unmarshal(created.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	dispatchBody, _ := json.Marshal(map[string]string{"input_revision_id": receipt.InputRevisionID})
	dispatchRecorder := httptest.NewRecorder()
	dispatchRequest := httptest.NewRequest(http.MethodPost, "/v1/teams/team/dispatch", bytes.NewReader(dispatchBody))
	dispatchRequest = dispatchRequest.WithContext(execution.WithSubject(dispatchRequest.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"}))
	c := echo.New().NewContext(dispatchRequest, dispatchRecorder)
	c.SetPath("/v1/teams/:id/dispatch")
	c.SetParamNames("id")
	c.SetParamValues("team")
	c.Set("tenant", "ws")
	c.Set("user_id", "user")
	if err := server.handleDispatchTeam(c); err != nil || dispatchRecorder.Code != http.StatusCreated {
		t.Fatalf("dispatch status=%d body=%s err=%v", dispatchRecorder.Code, dispatchRecorder.Body.String(), err)
	}
	var dispatch workflowManualRunResponse
	if err := json.Unmarshal(dispatchRecorder.Body.Bytes(), &dispatch); err != nil {
		t.Fatal(err)
	}
	claimed, err := server.Tasks.Claim(t.Context(), "test-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: "ws", IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim queued team run: %v", err)
	}
	consumer := &teamrun.Consumer{
		Transactions: pool,
		Snapshots:    server.Snapshots,
		Runs:         teamrun.NewPGStore(),
		Tasks:        server.Tasks,
	}
	established, err := consumer.ConsumeClaimed(t.Context(), claimed, "test-worker")
	if err != nil {
		t.Fatal(err)
	}
	if established.RunID != dispatch.RunID {
		t.Fatalf("established run=%q dispatch run=%q", established.RunID, dispatch.RunID)
	}
	tag, err := pool.Exec(t.Context(), `UPDATE weave_team_runs SET status='succeeded',updated_at=statement_timestamp(),terminal_at=statement_timestamp()
		WHERE workspace_id='ws' AND run_id=$1`, dispatch.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("terminal run %q was not updated", dispatch.RunID)
	}
	var candidateCount int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_team_runs AS run
		JOIN weave_dispatch_input_revisions AS input ON input.workspace_id=run.workspace_id AND input.consumed_run_id=run.run_id
		JOIN weave_external_identities AS identity ON identity.workspace_id=input.workspace_id AND identity.user_id=input.user_id
		WHERE run.workspace_id='ws' AND run.run_id=$1 AND run.status='succeeded'`, dispatch.RunID).Scan(&candidateCount); err != nil || candidateCount != 1 {
		t.Fatalf("terminal event candidate count=%d err=%v", candidateCount, err)
	}
	actions := &teamrun.PGActivityStore{Transactions: pool}
	writeActionEvent := func(phase, callID, actionName, actionLabel, recordID, status string) {
		t.Helper()
		kind := "business_action_started"
		if phase == "result" {
			kind = "business_action_result"
		}
		detail, err := json.Marshal(map[string]any{
			"source": "forge_mcp.run_action", "phase": phase, "invocation_id": "snapshot/0/lead", "tool_call_id": callID,
			"capability_id": "forge:action:sales_contract." + actionName,
			"action_key":    "sales_contract." + actionName, "action_name": actionName,
			"action_label": actionLabel, "object_name": "sales_contract",
			"input_revision_id": receipt.InputRevisionID, "record_id": recordID, "status": status,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := actions.RecordBusinessActionEvent(t.Context(), teamrun.ActivityEvent{
			WorkspaceID: "ws", RunID: dispatch.RunID, EventID: uuid.NewString(), Kind: kind,
			NodeID: "lead", MemberID: "lead-agent", MemberVersion: 1,
			Detail: detail, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeActionEvent("started", "call-action-1", "ContractSubmit", "提交指定合同版本", "private-record-reference", "")
	writeActionEvent("result", "call-action-1", "ContractSubmit", "提交指定合同版本", "private-record-reference", "succeeded")
	writeActionEvent("started", "call-action-2", "RequestRevision", "要求修订", "private-record-reference", "")
	var calls atomic.Int32
	forge := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer event-secret" {
			t.Errorf("missing service credential")
		}
		var event map[string]any
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Errorf("decode event: %v", err)
		}
		if event["kind"] != "result" || event["assigneeAccountId"] != "forge-user" || !strings.Contains(event["title"].(string), "已完成") {
			t.Errorf("unexpected event: %#v", event)
		}
		if len(event) != 9 {
			t.Errorf("native inbox event shape changed: %#v", event)
		}
		summary, _ := event["summary"].(string)
		if !strings.Contains(summary, "提交指定合同版本") || !strings.Contains(summary, "已确认完成") ||
			!strings.Contains(summary, "结果未知") || strings.Contains(summary, "sales_contract") || strings.Contains(summary, "private-record-reference") {
			t.Errorf("summary did not use safe platform action facts: %q", summary)
		}
		if _, exists := event["action_outcomes"]; exists {
			t.Errorf("native inbox event gained action_outcomes: %#v", event)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"notificationId":"notification-1","accepted":true}`))
	}))
	defer forge.Close()
	worker := &employeeRunEventWorker{
		Pool: pool, Endpoint: forge.URL, Secret: "event-secret",
		Client: forge.Client(), PollInterval: time.Millisecond,
	}
	processed, err := worker.Sweep(t.Context())
	if err != nil || processed != 1 {
		t.Fatalf("first sweep processed=%d err=%v", processed, err)
	}
	processed, err = worker.Sweep(t.Context())
	if err != nil || processed != 0 || calls.Load() != 1 {
		t.Fatalf("repeat sweep processed=%d calls=%d err=%v", processed, calls.Load(), err)
	}
	var state, notificationID string
	var attempts int
	if err := pool.QueryRow(t.Context(), `SELECT delivery_state,delivery_attempts,forge_notification_id
		FROM weave_employee_run_event_outbox WHERE workspace_id='ws' AND run_id=$1`, dispatch.RunID).Scan(&state, &attempts, &notificationID); err != nil {
		t.Fatal(err)
	}
	if state != "delivered" || attempts != 1 || notificationID != "notification-1" {
		t.Fatalf("unexpected outbox state=%s attempts=%d notification=%s", state, attempts, notificationID)
	}
}
