package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	seedTerminalRun := func(status string) (string, string) {
		t.Helper()
		runID, inputRevisionID := uuid.NewString(), uuid.NewString()
		taskID, registrationID, clientRequestID := uuid.NewString(), uuid.NewString(), uuid.NewString()
		sessionID := "workbench-session-" + status
		now := time.Now().UTC()
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		// Clone the entire frozen row so versioned fields such as snapshot_schema_version
		// and actor_subject remain valid under the current schema.
		if _, err := tx.Exec(t.Context(), `INSERT INTO weave_team_run_snapshots
		SELECT (jsonb_populate_record(
			NULL::weave_team_run_snapshots,
			to_jsonb(snapshot) || jsonb_build_object('run_id',$2)
		)).*
		FROM weave_team_runs AS run
		JOIN weave_team_run_snapshots AS snapshot
		  ON snapshot.workspace_id=run.workspace_id AND snapshot.run_id=run.run_snapshot_id
		WHERE run.workspace_id='ws' AND run.run_id=$1`, dispatch.RunID, runID); err != nil {
			t.Fatalf("copy frozen run snapshot for %s event: %v", status, err)
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO weave_team_runs (
			workspace_id,run_id,status,team_run_generation,execution_lease_epoch,resume_generation,
			team_id,workflow_id,workflow_version,run_snapshot_id,source_kind,source_task_id,
			establish_idempotency_key,created_at,updated_at,terminal_at,error_code,cause_summary
		)
		SELECT run.workspace_id,$2,$3,run.team_run_generation,run.execution_lease_epoch,run.resume_generation,
			run.team_id,run.workflow_id,run.workflow_version,$2,run.source_kind,$4,
			'employee-run-event-test:'||$2,$5,$5,$5,
			CASE $3 WHEN 'failed' THEN 'team_run_execution_failed' WHEN 'cancelled' THEN 'team_run_cancelled' END,
			CASE $3 WHEN 'failed' THEN 'synthetic failure' END
		FROM weave_team_runs AS run WHERE run.workspace_id='ws' AND run.run_id=$1`,
			dispatch.RunID, runID, status, taskID, now); err != nil {
			t.Fatalf("seed %s terminal run: %v", status, err)
		}
		// Preserve the current frozen input shape while assigning a distinct
		// session, revision, admission, and consumed-run identity.
		if _, err := tx.Exec(t.Context(), `INSERT INTO weave_dispatch_input_revisions
		SELECT (jsonb_populate_record(
			NULL::weave_dispatch_input_revisions,
			to_jsonb(input) || jsonb_build_object(
				'workbench_session_id',$2,'input_revision_id',$3,'registration_id',$4,
				'client_request_id',$5,'is_current',true,'consumed_run_id',$6,
				'consumed_task_id',$7,'created_at',$8,'consumed_at',$8,'closed_at',null
			)
		)).*
		FROM weave_dispatch_input_revisions AS input
		WHERE input.workspace_id='ws' AND input.input_revision_id=$1`,
			receipt.InputRevisionID, sessionID, inputRevisionID, registrationID,
			clientRequestID, runID, taskID, now); err != nil {
			t.Fatalf("bind %s terminal event to originating employee: %v", status, err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("commit %s terminal fixture: %v", status, err)
		}
		return runID, inputRevisionID
	}
	failedRunID, failedInputRevisionID := seedTerminalRun("failed")
	cancelledRunID, cancelledInputRevisionID := seedTerminalRun("cancelled")
	revisionRequiredRunID, revisionRequiredInputRevisionID := seedTerminalRun("succeeded")
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_final_deliverables
		(id,workspace_id,user_id,lead_avatar_id,session_id,event_id,run_id,run_snapshot_id,title,content,content_type,metadata)
		VALUES($1,'ws','user','lead','workbench-session-succeeded',$2,$3,$3,'本轮检查意见',$4,'application/json',
		'{"artifact_kind":"final","workbench_result":{"protocol":"workbench_result_v1","disposition":"needs_input","summary":"缺少原始签署日期","missing_items":["提供完整签署日期"]}}'::jsonb)`,
		"deliverable-revision-required", uuid.NewString(), revisionRequiredRunID,
		`{"disposition":"needs_input","summary":"缺少原始签署日期","missing_items":["提供完整签署日期"]}`); err != nil {
		t.Fatal(err)
	}
	terminalKinds := map[string]string{
		dispatch.RunID:        "result",
		failedRunID:           "failure",
		cancelledRunID:        "cancelled",
		revisionRequiredRunID: "revision_required",
	}
	inputReferences := map[string]string{
		dispatch.RunID:        receipt.InputRevisionID,
		failedRunID:           failedInputRevisionID,
		cancelledRunID:        cancelledInputRevisionID,
		revisionRequiredRunID: revisionRequiredInputRevisionID,
	}
	actions := &teamrun.PGActivityStore{Transactions: pool}
	writeRunActionEvent := func(runID, phase, callID, actionName, actionLabel, recordID, status string) {
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
			WorkspaceID: "ws", RunID: runID, EventID: uuid.NewString(), Kind: kind,
			NodeID: "lead", MemberID: "lead-agent", MemberVersion: 1,
			Detail: detail, OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	writeActionEvent := func(phase, callID, actionName, actionLabel, recordID, status string) {
		writeRunActionEvent(dispatch.RunID, phase, callID, actionName, actionLabel, recordID, status)
	}
	writeActionEvent("started", "call-action-1", "ContractSubmit", "提交指定合同版本", "private-record-reference", "")
	writeActionEvent("result", "call-action-1", "ContractSubmit", "提交指定合同版本", "private-record-reference", "succeeded")
	writeActionEvent("started", "call-action-2", "RequestRevision", "要求修订", "private-record-reference", "")
	for index := 3; index <= 12; index++ {
		callID := fmt.Sprintf("call-action-%d", index)
		actionName := fmt.Sprintf("Step%d", index)
		label := fmt.Sprintf("处理步骤%d", index)
		writeActionEvent("started", callID, actionName, label, "private-record-reference", "")
		writeActionEvent("result", callID, actionName, label, "private-record-reference", "succeeded")
	}
	writeActionEvent("started", "call-action-13", "FinalReject", "最后失败动作", "private-record-reference", "")
	writeActionEvent("result", "call-action-13", "FinalReject", "最后失败动作", "private-record-reference", "failed")
	writeActionEvent("started", "call-action-14", "FinalUnknown", "最后未知动作", "private-record-reference", "")
	writeRunActionEvent(failedRunID, "started", "failed-call-1", "RequestRevision", "提交修订", "private-record-reference", "")
	writeRunActionEvent(failedRunID, "result", "failed-call-1", "RequestRevision", "提交修订", "private-record-reference", "failed")
	writeRunActionEvent(failedRunID, "started", "failed-call-2", "ContractSubmit", "提交合同", "private-record-reference", "")
	var calls atomic.Int32
	var receiverMu sync.Mutex
	seenRunEvents := make(map[string]int)
	eventIDByIdempotencyKey := make(map[string]string)
	notificationByIdempotencyKey := make(map[string]string)
	forge := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		callNumber := calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer event-secret" {
			t.Errorf("missing service credential")
		}
		var event map[string]any
		if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
			t.Errorf("decode event: %v", err)
		}
		source, ok := event["source"].(map[string]any)
		if !ok {
			t.Errorf("missing source references: %#v", event)
			return
		}
		runReference, _ := source["runReference"].(string)
		idempotencyKey, _ := source["idempotencyKey"].(string)
		wantKind, knownRun := terminalKinds[runReference]
		if !knownRun || event["kind"] != wantKind || event["assigneeAccountId"] != "forge-user" {
			t.Errorf("unexpected event: %#v", event)
		}
		if source["workReference"] != inputReferences[runReference] || idempotencyKey != "weave-team-run-terminal:"+runReference {
			t.Errorf("event was not bound to its originating work and run: %#v", event)
		}
		if event["eventId"] == "" || event["title"] == "" {
			t.Errorf("event identity or title is missing: %#v", event)
		}
		if len(event) != 9 {
			t.Errorf("native inbox event shape changed: %#v", event)
		}
		if runReference == dispatch.RunID {
			if event["title"] != "团队运行已完成（业务动作需核对）：flow" {
				t.Errorf("success event title did not separate run completion from action issues: %#v", event)
			}
			summary, _ := event["summary"].(string)
			if !strings.Contains(summary, "团队运行状态：已完成") ||
				!strings.Contains(summary, "业务动作调用结果：成功 11 项，失败 1 项，结果未知 2 项") ||
				!strings.Contains(summary, "失败或未知结果请先核对 Forge 业务记录后再决定下一步") ||
				!strings.Contains(summary, "本消息不代表正式审批状态") || strings.Contains(summary, "续办") ||
				strings.Contains(summary, "重试") || strings.Contains(summary, "sales_contract") || strings.Contains(summary, "private-record-reference") {
				t.Errorf("summary did not use safe platform action facts: %q", summary)
			}
		} else if runReference == failedRunID {
			if event["title"] != "团队运行失败（业务动作需核对）：flow" {
				t.Errorf("failure event title did not separate run and action status: %#v", event)
			}
			summary, _ := event["summary"].(string)
			if !strings.Contains(summary, "团队运行状态：失败") ||
				!strings.Contains(summary, "业务动作“提交修订”调用返回失败，请先核对业务记录后再处理") ||
				!strings.Contains(summary, "业务动作“提交合同”结果未知，请先核对业务记录后再处理") ||
				!strings.Contains(summary, "正式审批状态请以 Forge 业务记录为准") {
				t.Errorf("summary did not separate run failure from action outcomes: %q", summary)
			}
		} else if runReference == revisionRequiredRunID {
			if event["title"] != "团队运行需要补充材料：flow" {
				t.Errorf("revision-required event title did not identify required input: %#v", event)
			}
			summary, _ := event["summary"].(string)
			if !strings.Contains(summary, "团队检查结论：缺少原始签署日期") || !strings.Contains(summary, "需要补充：提供完整签署日期") {
				t.Errorf("revision-required event did not include structured inspection facts: %q", summary)
			}
		} else if runReference == cancelledRunID && event["title"] != "团队运行已取消：flow" {
			t.Errorf("cancel event title did not identify cancellation: %#v", event)
		}
		if _, exists := event["action_outcomes"]; exists {
			t.Errorf("native inbox event gained action_outcomes: %#v", event)
		}
		eventID, _ := event["eventId"].(string)
		receiverMu.Lock()
		seenRunEvents[runReference]++
		if prior, exists := eventIDByIdempotencyKey[idempotencyKey]; exists && prior != eventID {
			t.Errorf("retry changed event id for idempotency key %q: %q != %q", idempotencyKey, eventID, prior)
		}
		eventIDByIdempotencyKey[idempotencyKey] = eventID
		notificationID, exists := notificationByIdempotencyKey[idempotencyKey]
		if !exists {
			notificationID = fmt.Sprintf("notification-%d", len(notificationByIdempotencyKey)+1)
			notificationByIdempotencyKey[idempotencyKey] = notificationID
		}
		receiverMu.Unlock()
		if callNumber == 1 {
			// Model the receiver committing its inbox row before an intermediary
			// loses the acknowledgement. The retry must carry the same key.
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(fmt.Sprintf(`{"notificationId":%q,"accepted":true}`, notificationID)))
	}))
	defer forge.Close()
	worker := &employeeRunEventWorker{
		Pool: pool, Endpoint: forge.URL, Secret: "event-secret",
		Client: forge.Client(), PollInterval: time.Millisecond,
	}
	for attempt := 0; attempt < 5; attempt++ {
		processed, err := worker.Sweep(t.Context())
		if err != nil || processed != 1 {
			t.Fatalf("sweep %d processed=%d err=%v", attempt+1, processed, err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE weave_employee_run_event_outbox SET next_attempt_at=statement_timestamp()
			WHERE delivery_state='pending'`); err != nil {
			t.Fatalf("make retry immediately claimable: %v", err)
		}
	}
	processed, err := worker.Sweep(t.Context())
	if err != nil || processed != 0 || calls.Load() != 5 {
		t.Fatalf("repeat sweep processed=%d calls=%d err=%v", processed, calls.Load(), err)
	}
	var outboxCount, deliveredCount, totalAttempts int
	if err := pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE delivery_state='delivered'),sum(delivery_attempts)
		FROM weave_employee_run_event_outbox WHERE workspace_id='ws' AND run_id=ANY($1::text[])`,
		[]string{dispatch.RunID, failedRunID, revisionRequiredRunID, cancelledRunID}).Scan(&outboxCount, &deliveredCount, &totalAttempts); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 4 || deliveredCount != 4 || totalAttempts != 5 {
		t.Fatalf("outbox rows=%d delivered=%d total attempts=%d, want 4, 4, 5", outboxCount, deliveredCount, totalAttempts)
	}
	receiverMu.Lock()
	if len(notificationByIdempotencyKey) != 4 {
		t.Errorf("receiver created %d inbox rows, want one per terminal run", len(notificationByIdempotencyKey))
	}
	for runID := range terminalKinds {
		if seenRunEvents[runID] == 0 {
			t.Errorf("no terminal event delivered for run %q", runID)
		}
	}
	receiverMu.Unlock()
	for _, runID := range []string{dispatch.RunID, failedRunID, revisionRequiredRunID, cancelledRunID} {
		var state, notificationID string
		var attempts int
		if err := pool.QueryRow(t.Context(), `SELECT delivery_state,delivery_attempts,forge_notification_id
			FROM weave_employee_run_event_outbox WHERE workspace_id='ws' AND run_id=$1`, runID).Scan(&state, &attempts, &notificationID); err != nil {
			t.Fatal(err)
		}
		wantAttempts := 1
		if seenRunEvents[runID] == 2 {
			wantAttempts = 2
		}
		if state != "delivered" || attempts != wantAttempts || notificationID == "" {
			t.Errorf("unexpected outbox state for run %s: state=%s attempts=%d notification=%q", runID, state, attempts, notificationID)
		}
	}
}
