package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

func callWorkbenchRunLookup(t *testing.T, server *Server, userID string, body any) (int, workbenchRunLookupResponse) {
	t.Helper()
	encoded, _ := json.Marshal(body)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/workbench/runs/lookup", bytes.NewReader(encoded))
	c := echo.New().NewContext(request, recorder)
	c.Set("tenant", "ws")
	c.Set("user_id", userID)
	if err := server.handleLookupWorkbenchRuns(c); err != nil {
		t.Fatal(err)
	}
	var response workbenchRunLookupResponse
	if recorder.Code == http.StatusOK {
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
	}
	return recorder.Code, response
}

// Decision 002 / C22: the desktop asks Weave whether a run's input is still
// current and how the run ended, instead of deriving it from message order.
func TestWorkbenchRunLookupProjectsOwnRunsOnlyRealPG(t *testing.T) {
	server, pool, runID := succeededWorkbenchRunForTest(t, "workbench-user")
	server.teamRunActivities = &teamrun.PGActivityStore{Transactions: pool}

	status, response := callWorkbenchRunLookup(t, server, "user", map[string]any{"version": "1", "runIds": []string{runID, "missing-run"}})
	if status != http.StatusOK || len(response.Runs) != 1 || len(response.Missing) != 1 || response.Missing[0] != "missing-run" {
		t.Fatalf("lookup status=%d response=%+v", status, response)
	}
	run := response.Runs[0]
	if run.RunID != runID || run.Status != "succeeded" || run.BusinessResult != "completed" || !run.IsCurrent ||
		run.InputRevisionID == "" || run.WorkbenchSessionID != "workbench-session" || run.ActionCounts != (workbenchRunLookupCounts{}) {
		t.Fatalf("unexpected projection: %+v", run)
	}

	// Another employee learns nothing about the run.
	if status, other := callWorkbenchRunLookup(t, server, "another-user", map[string]any{"version": "1", "runIds": []string{runID}}); status != http.StatusOK || len(other.Runs) != 0 || len(other.Missing) != 1 {
		t.Fatalf("foreign lookup status=%d response=%+v", status, other)
	}

	// A later input in the same session supersedes this run's input.
	if _, err := pool.Exec(t.Context(), `UPDATE weave_dispatch_input_revisions SET is_current=false
		WHERE workspace_id='ws' AND user_id='user' AND input_revision_id=$1`, run.InputRevisionID); err != nil {
		t.Fatal(err)
	}
	if _, after := callWorkbenchRunLookup(t, server, "user", map[string]any{"version": "1", "runIds": []string{runID}}); len(after.Runs) != 1 || after.Runs[0].IsCurrent {
		t.Fatalf("superseded input still reported current: %+v", after)
	}

	for _, invalid := range []any{
		map[string]any{"version": "1", "runIds": []string{}},
		map[string]any{"version": "1", "runIds": []string{runID, runID}},
		map[string]any{"version": "2", "runIds": []string{runID}},
		map[string]any{"version": "1", "runIds": []string{runID}, "extra": true},
	} {
		if status, _ := callWorkbenchRunLookup(t, server, "user", invalid); status != http.StatusBadRequest {
			t.Fatalf("invalid lookup %v status=%d", invalid, status)
		}
	}
}
