package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

// succeededRunForOutboxTest dispatches one real team run and marks it
// succeeded; the worker then materializes its outbox event exactly as in
// production. It returns the pool and the run ID.
func succeededRunForOutboxTest(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	_, pool, runID := succeededWorkbenchRunForTest(t, "")
	return pool, runID
}

// succeededWorkbenchRunForTest dispatches one input under projectID and marks
// its run succeeded, returning the server that owns it.
func succeededWorkbenchRunForTest(t *testing.T, projectID string) (*Server, *pgxpool.Pool, string) {
	t.Helper()
	server, pool := newTeamDispatchTestServer(t)
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_external_identities(issuer,subject,workspace_id,user_id,native_organization)
		VALUES('forge:event-test','forge-user','ws','user','native-event-org')`); err != nil {
		t.Fatal(err)
	}
	registration := dispatchInputRegistrationFixture("workbench-session", "检查固定材料", "")
	registration.ProjectID = projectID
	created, err := registerInputForTest(server, registration)
	if err != nil || created.Code != http.StatusCreated {
		t.Fatalf("register input status=%d body=%s err=%v", created.Code, created.Body.String(), err)
	}
	var receipt dispatchInputReceipt
	if err := json.Unmarshal(created.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"input_revision_id": receipt.InputRevisionID})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/teams/team/dispatch", bytes.NewReader(body))
	request = request.WithContext(execution.WithSubject(request.Context(), execution.Subject{WorkspaceID: "ws", UserID: "user"}))
	c := echo.New().NewContext(request, recorder)
	c.SetPath("/v1/teams/:id/dispatch")
	c.SetParamNames("id")
	c.SetParamValues("team")
	c.Set("tenant", "ws")
	c.Set("user_id", "user")
	if err := server.handleDispatchTeam(c); err != nil || recorder.Code != http.StatusCreated {
		t.Fatalf("dispatch status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
	var dispatch workflowManualRunResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &dispatch); err != nil {
		t.Fatal(err)
	}
	claimed, err := server.Tasks.Claim(t.Context(), "test-worker", taskqueue.ClaimFilter{
		Kind: "team_workflow", WorkspaceID: "ws", IdentityKind: taskqueue.IdentityTeamWorkflow,
	})
	if err != nil || claimed == nil {
		t.Fatalf("claim queued team run: %v", err)
	}
	consumer := &teamrun.Consumer{Transactions: pool, Snapshots: server.Snapshots, Runs: teamrun.NewPGStore(), Tasks: server.Tasks}
	if _, err := consumer.ConsumeClaimed(t.Context(), claimed, "test-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_team_runs SET status='succeeded',updated_at=statement_timestamp(),terminal_at=statement_timestamp()
		WHERE workspace_id='ws' AND run_id=$1`, dispatch.RunID); err != nil {
		t.Fatal(err)
	}
	return server, pool, dispatch.RunID
}

func outboxState(t *testing.T, pool *pgxpool.Pool, runID string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT delivery_state FROM weave_employee_run_event_outbox
		WHERE workspace_id='ws' AND run_id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read outbox state: %v", err)
	}
	return state
}

// A wrong event secret makes Forge answer 401 for every result. Those events
// become permanent failures that nothing retries and no employee sees; the
// operator must be able to list them and, after fixing the cause, redeliver.
func TestPermanentEmployeeRunEventFailureCanBeListedAndRedeliveredRealPG(t *testing.T) {
	pool, runID := succeededRunForOutboxTest(t)
	var forgeStatus atomic.Int32
	forgeStatus.Store(http.StatusUnauthorized)
	var accepted atomic.Int32
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status := int(forgeStatus.Load())
		w.WriteHeader(status)
		if status == http.StatusAccepted {
			accepted.Add(1)
			_, _ = w.Write([]byte(`{"notificationId":"notification-1","accepted":true}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"code":"UNAUTHORIZED"}}`))
	}))
	defer forge.Close()
	worker := &employeeRunEventWorker{Pool: pool, Endpoint: forge.URL, Secret: "secret", Client: forge.Client(), PollInterval: time.Second}

	if swept, err := worker.Sweep(t.Context()); err != nil || swept != 1 {
		t.Fatalf("first sweep swept=%d err=%v", swept, err)
	}
	if state := outboxState(t, pool, runID); state != "permanent_failure" {
		t.Fatalf("a 401 from Forge left the event %q, want permanent_failure", state)
	}
	if swept, err := worker.Sweep(t.Context()); err != nil || swept != 0 {
		t.Fatalf("a permanent failure was picked up again without an operator: swept=%d err=%v", swept, err)
	}

	failures, err := ListEmployeeRunEventFailures(t.Context(), pool)
	if err != nil || len(failures) != 1 || failures[0].RunID != runID || failures[0].WorkspaceID != "ws" ||
		failures[0].LastError == "" || failures[0].Attempts < 1 {
		t.Fatalf("operator list = %+v err=%v", failures, err)
	}

	if _, err := RequeueEmployeeRunEventFailures(t.Context(), pool, []string{"not-a-uuid"}); err == nil {
		t.Fatal("a malformed event ID was passed to the database")
	}
	// The operator fixes the secret; Forge now accepts the event.
	forgeStatus.Store(http.StatusAccepted)
	if moved, err := RequeueEmployeeRunEventFailures(t.Context(), pool, []string{uuid.NewString()}); err != nil || moved != 0 {
		t.Fatalf("requeue of an unknown event moved %d rows err=%v", moved, err)
	}
	if state := outboxState(t, pool, runID); state != "permanent_failure" {
		t.Fatalf("requeue of another event changed this one to %q", state)
	}
	moved, err := RequeueEmployeeRunEventFailures(t.Context(), pool, []string{failures[0].EventID})
	if err != nil || moved != 1 {
		t.Fatalf("requeue by ID moved %d rows err=%v", moved, err)
	}
	if swept, err := worker.Sweep(t.Context()); err != nil || swept != 1 {
		t.Fatalf("redelivery sweep swept=%d err=%v", swept, err)
	}
	if state := outboxState(t, pool, runID); state != "delivered" || accepted.Load() != 1 {
		t.Fatalf("after redelivery state=%q accepted=%d, want delivered and exactly one accepted request", state, accepted.Load())
	}
	if remaining, err := ListEmployeeRunEventFailures(t.Context(), pool); err != nil || len(remaining) != 0 {
		t.Fatalf("a delivered event is still listed as failed: %+v err=%v", remaining, err)
	}
	// Delivered events are never requeued, whether targeted or swept in bulk.
	if moved, err := RequeueEmployeeRunEventFailures(t.Context(), pool, nil); err != nil || moved != 0 {
		t.Fatalf("bulk requeue touched a delivered event: moved=%d err=%v", moved, err)
	}
	if state := outboxState(t, pool, runID); state != "delivered" {
		t.Fatalf("bulk requeue changed a delivered event to %q", state)
	}
}
