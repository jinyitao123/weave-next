package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/loom/pgstore"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

func TestWorkbenchActivitySummaryPreservesAcceptedQueuedRun(t *testing.T) {
	startedAt := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	summary := workbenchRunActivitySummary(workbenchRunRecord{
		RunID: "run-a", Status: "queued", ProjectID: "project-real", TeamID: "team-a",
		WorkflowID: "flow-a", WorkflowVersion: 1, StartedAt: startedAt, UpdatedAt: startedAt,
	})
	if summary["run_id"] != "run-a" || summary["status"] != "queued" || summary["team_id"] != "team-a" {
		t.Fatalf("accepted run identity/status = %#v", summary)
	}
	if len(summary["members"].([]runActivityMember)) != 0 || summary["delivery"].(map[string]any)["reason"] != "awaiting_delivery" {
		t.Fatalf("queued run invented execution or delivery facts: %#v", summary)
	}
}

func TestWorkbenchRunsUseAuthenticatedInputOwnerAndMatchDetailsRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	connection, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	query := connection.Query()
	query.Set("search_path", pool.Config().ConnConfig.RuntimeParams["search_path"])
	connection.RawQuery = query.Encode()
	store, err := pgstore.New(connection.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	server.Store = teamDispatchPoolStore{Store: store, pool: pool}
	dispatch := func(userID string) workflowManualRunResponse {
		t.Helper()
		registration := dispatchInputRegistrationFixture("workbench-session-"+userID, "检查一份固定材料", "")
		version := 1
		registration.WorkflowID, registration.WorkflowVersion = "flow", &version
		registration.ProjectID = workbenchProjectID(userID)
		body, err := json.Marshal(registration)
		if err != nil {
			t.Fatal(err)
		}
		ctx, recorder := dispatchInputTestContext(body, "/v1/workbench/dispatch-inputs", "ws", userID)
		if err := server.handleRegisterDispatchInput(ctx); err != nil || recorder.Code != http.StatusCreated {
			t.Fatalf("register %s: status=%d body=%s err=%v", userID, recorder.Code, recorder.Body.String(), err)
		}
		var input dispatchInputReceipt
		if err := json.Unmarshal(recorder.Body.Bytes(), &input); err != nil {
			t.Fatal(err)
		}
		dispatched, err := boundDispatchForTest(server, map[string]any{"input_revision_id": input.InputRevisionID, "client_request_id": input.ClientRequestID}, userID)
		if err != nil || dispatched.Code != http.StatusCreated {
			t.Fatalf("dispatch %s: status=%d body=%s err=%v", userID, dispatched.Code, dispatched.Body.String(), err)
		}
		var receipt workflowManualRunResponse
		if err := json.Unmarshal(dispatched.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.ProjectID == registration.ProjectID {
			t.Fatalf("compatibility project alias was persisted as the run project: %+v", receipt)
		}
		return receipt
	}
	runA, runB := dispatch("user-a"), dispatch("user-b")
	if runA.ProjectID != runB.ProjectID {
		t.Fatalf("fixture did not reproduce the shared team project: %q != %q", runA.ProjectID, runB.ProjectID)
	}

	// Admission has persisted the task and input receipt; the worker has not yet
	// established TeamRun for A. Once it does, that state must supersede queue state.
	runs := teamrun.NewPGStore()
	runs.Transactions = pool
	server.teamRunCancel = &teamrun.CancelService{Transactions: pool, Runs: runs, Tasks: server.Tasks}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	server.teamRunCancel.Now = func() time.Time { return now.Add(2 * time.Second) }
	if _, err := runs.EstablishQueuedTx(t.Context(), tx, teamrun.EstablishRequest{
		WorkspaceID: "ws", ProjectID: runA.ProjectID, RunID: runA.RunID, TeamID: "team", WorkflowID: "flow", WorkflowVersion: 1,
		RunSnapshotID: runA.RunID, SourceKind: teamrun.SourceManual, SourceTaskID: runA.TaskID,
		EstablishIdempotencyKey: "establish-" + runA.TaskID, Actor: "fixture", Source: "fixture", OccurredAt: now,
	}); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if _, err := runs.ClaimRunningTx(t.Context(), tx, teamrun.ClaimRequest{
		WorkspaceID: "ws", RunID: runA.RunID, ExpectedStatus: teamrun.StatusQueued,
		ExecutorID: "fixture-worker", IdempotencyKey: "claim-" + runA.TaskID,
		Actor: "fixture", Source: "fixture", OccurredAt: now.Add(time.Second),
	}); err != nil {
		_ = tx.Rollback(t.Context())
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	list := func(userID, projectID string, offset int) (int, RunListResponse) {
		t.Helper()
		query := url.Values{"project_id": {projectID}, "limit": {"10"}, "offset": {strconv.Itoa(offset)}}
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/runs?"+query.Encode(), nil)
		c := echo.New().NewContext(request, recorder)
		c.Set("tenant", "ws")
		c.Set("user_id", userID)
		if err := server.handleListRuns(c); err != nil {
			t.Fatal(err)
		}
		var response RunListResponse
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return recorder.Code, response
	}
	statusA, listA := list("user-a", workbenchProjectID("user-a"), 0)
	statusB, listB := list("user-b", workbenchProjectID("user-b"), 0)
	if statusA != http.StatusOK || statusB != http.StatusOK || listA.Total != 1 || listB.Total != 1 || len(listA.Runs) != 1 || len(listB.Runs) != 1 {
		t.Fatalf("account lists differ: A=%d %+v B=%d %+v", statusA, listA, statusB, listB)
	}
	if listA.Runs[0].RunID != runA.RunID || listA.Runs[0].Status != "running" ||
		listB.Runs[0].RunID != runB.RunID || listB.Runs[0].Status != "queued" {
		t.Fatalf("list did not project owned authoritative state: A=%+v B=%+v", listA.Runs[0], listB.Runs[0])
	}
	if status, _ := list("user-a", workbenchProjectID("user-b"), 0); status != http.StatusForbidden {
		t.Fatalf("account A accepted account B project alias: status=%d", status)
	}
	if code, emptyPage := list("user-a", workbenchProjectID("user-a"), 1); code != http.StatusOK || emptyPage.Total != 1 || len(emptyPage.Runs) != 0 {
		t.Fatalf("empty page lost owner count: status=%d response=%+v", code, emptyPage)
	}

	detail := func(userID, runID string) (int, map[string]any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID, nil)
		c := echo.New().NewContext(request, recorder)
		c.Set("tenant", "ws")
		c.Set("user_id", userID)
		c.SetParamNames("id")
		c.SetParamValues(runID)
		if err := server.handleGetRun(c); err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return recorder.Code, response
	}
	if code, detailA := detail("user-a", runA.RunID); code != http.StatusOK || detailA["run_id"] != runA.RunID || detailA["status"] != listA.Runs[0].Status {
		t.Fatalf("A detail disagrees with A list: status=%d detail=%+v", code, detailA)
	}
	if code, detailB := detail("user-b", runB.RunID); code != http.StatusOK || detailB["run_id"] != runB.RunID || detailB["status"] != listB.Runs[0].Status {
		t.Fatalf("B detail disagrees with B list: status=%d detail=%+v", code, detailB)
	}
	if code, _ := detail("user-b", runA.RunID); code != http.StatusNotFound {
		t.Fatalf("account B read account A run detail: status=%d", code)
	}
	activity := func(userID, runID string) (int, map[string]any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/activity", nil)
		c := echo.New().NewContext(request, recorder)
		c.Set("tenant", "ws")
		c.Set("user_id", userID)
		c.SetParamNames("id")
		c.SetParamValues(runID)
		if err := server.handleGetRunActivity(c); err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if recorder.Code == http.StatusOK {
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
		}
		return recorder.Code, response
	}
	if code, activityA := activity("user-a", runA.RunID); code != http.StatusOK || activityA["run_id"] != runA.RunID || activityA["status"] != listA.Runs[0].Status {
		t.Fatalf("A activity disagrees with A list: status=%d activity=%+v", code, activityA)
	}
	if code, activityB := activity("user-b", runB.RunID); code != http.StatusOK || activityB["run_id"] != runB.RunID || activityB["status"] != listB.Runs[0].Status {
		t.Fatalf("B queued activity disagrees with B list: status=%d activity=%+v", code, activityB)
	}
	if code, _ := activity("user-b", runA.RunID); code != http.StatusNotFound {
		t.Fatalf("account B read account A activity: status=%d", code)
	}
	stop := func(userID string) int {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/runs/"+runA.RunID+"/stop",
			strings.NewReader(`{"reason":"employee_cancel","idempotency_key":"stop-owned"}`))
		c := echo.New().NewContext(request, recorder)
		c.Set("tenant", "ws")
		c.Set("user_id", userID)
		c.Set("roles", []string{"developer"})
		c.SetParamNames("id")
		c.SetParamValues(runA.RunID)
		if err := server.handleStopRun(c); err != nil {
			t.Fatal(err)
		}
		return recorder.Code
	}
	if status := stop("user-b"); status != http.StatusNotFound {
		t.Fatalf("same-workspace developer stopped another employee's run: %d", status)
	}
	if code, current := detail("user-a", runA.RunID); code != http.StatusOK || current["status"] != "running" {
		t.Fatalf("denied stop changed run: %d %+v", code, current)
	}
	for range 2 {
		if status := stop("user-a"); status != http.StatusAccepted {
			t.Fatalf("owner stop/replay failed: %d", status)
		}
	}
}
