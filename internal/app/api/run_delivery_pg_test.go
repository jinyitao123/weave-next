package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/teamrun"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/labstack/echo/v4"
)

func TestRunDeliveryRecheckAndHistoryAPIRealPG(t *testing.T) {
	f := newTeamDeliveryFixture(t, engine.Claude)
	executed := f.run(t, teamDeliveryScenario{name: "recheck API", want: deliverable.VerificationPassed, reason: "isolated_effects_match"})
	var queueBefore, filesBefore int
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM weave_task_queue),(SELECT count(*) FROM weave_final_deliverables)`).Scan(&queueBefore, &filesBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO fixture_delivery_objects VALUES ('ws',$1,'unexpected',42)`, executed.runID); err != nil {
		t.Fatal(err)
	}
	invoke := func(tenant, runID, method, reportID string, body any, handler echo.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		recorded := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/v1/runs/"+runID+"/delivery", bytes.NewReader(raw)).WithContext(t.Context())
		request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		c := echo.New().NewContext(request, recorded)
		c.Set("tenant", tenant)
		c.Set("user_id", "user")
		c.SetParamNames("id", "verification_id")
		c.SetParamValues(runID, reportID)
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		return recorded
	}
	request := map[string]string{"revision_id": executed.state.RevisionID, "contract_digest": executed.state.ContractDigest}
	response := invoke("ws", executed.runID, http.MethodPost, "", request, f.server.handleRecheckRunDelivery)
	if response.Code != http.StatusOK {
		t.Fatalf("recheck=%d %s", response.Code, response.Body.String())
	}
	var result deliverable.RecheckResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Current || result.Report.Status != deliverable.VerificationFailed || result.Report.RevisionID != executed.state.RevisionID || result.Report.ID == executed.state.VerificationID {
		t.Fatalf("recheck did not independently observe the existing revision: %+v", result)
	}
	old := invoke("ws", executed.runID, http.MethodGet, executed.state.VerificationID, nil, f.server.handleGetRunVerification)
	var oldReport deliverable.VerificationReport
	if old.Code != http.StatusOK || json.Unmarshal(old.Body.Bytes(), &oldReport) != nil || oldReport.Status != deliverable.VerificationPassed {
		t.Fatalf("old report overwritten: %d %s", old.Code, old.Body.String())
	}
	for _, handler := range []echo.HandlerFunc{f.server.handleGetRunDelivery, f.server.handleGetRunVerification} {
		if denied := invoke("other-workspace", executed.runID, http.MethodGet, result.Report.ID, nil, handler); denied.Code != http.StatusNotFound {
			t.Fatalf("other workspace read delivery=%d %s", denied.Code, denied.Body.String())
		}
	}
	if denied := invoke("ws", "other-run", http.MethodGet, result.Report.ID, nil, f.server.handleGetRunVerification); denied.Code != http.StatusNotFound {
		t.Fatalf("other run read report=%d", denied.Code)
	}
	if rejected := invoke("ws", executed.runID, http.MethodPost, "", map[string]string{"revision_id": "old-revision", "contract_digest": executed.state.ContractDigest}, f.server.handleRecheckRunDelivery); rejected.Code != http.StatusConflict {
		t.Fatalf("old revision accepted=%d", rejected.Code)
	}
	if rejected := invoke("ws", executed.runID, http.MethodPost, "", map[string]string{"revision_id": executed.state.RevisionID, "contract_digest": executed.state.ContractDigest, "status": "passed"}, f.server.handleRecheckRunDelivery); rejected.Code != http.StatusBadRequest {
		t.Fatalf("caller verdict accepted=%d", rejected.Code)
	}
	current, err := executed.store.GetDeliveryState(t.Context(), "ws", executed.runID)
	if err != nil || current.VerificationID != result.Report.ID {
		t.Fatalf("rejected request changed current report: %+v %v", current, err)
	}
	var queueAfter, filesAfter int
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM weave_task_queue),(SELECT count(*) FROM weave_final_deliverables)`).Scan(&queueAfter, &filesAfter); err != nil {
		t.Fatal(err)
	}
	run, err := f.runs.Get(t.Context(), "ws", executed.runID)
	if err != nil || run.Status != teamrun.StatusSucceeded || queueBefore != queueAfter || filesBefore != filesAfter {
		t.Fatalf("read-only recheck changed execution or artifacts: run=%+v queue=%d/%d files=%d/%d %v", run, queueBefore, queueAfter, filesBefore, filesAfter, err)
	}
}
