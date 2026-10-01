package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/app/kernelbindings"
	"github.com/jinyitao123/weave/internal/kernel/teamrun"
	"github.com/labstack/echo/v4"
)

func TestWorkbenchHumanTaskRequiresOwnerAndFourSourceReferencesRealPG(t *testing.T) {
	server, pool, runID := succeededWorkbenchRunForTest(t, workbenchProjectID("user"))
	server.OrgStore = kernelbindings.NewOrganization(pool)
	detail := `{"schema_version":1,"wait_type":"human","node_id":"confirm","success_node_id":"deliver","resume_schema":{"type":"object"},"task":{"title":"确认合同条款","instructions":"请核对条款后继续"}}`
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO weave_members(workspace_id,user_id,role) VALUES('ws','user','member'),('ws','user-other','member') ON CONFLICT DO NOTHING;
		UPDATE weave_team_runs SET status='parked',terminal_at=NULL,wait_kind='human',wait_detail=$2::jsonb,
		  resume_token_hash='\x01'::bytea,checkpoint_ref=$3,updated_at=statement_timestamp()
		  WHERE workspace_id='ws' AND run_id=$1`, runID, detail, teamrun.CheckpointRef("ws", runID)); err != nil {
		t.Fatal(err)
	}
	runs, checkpoints := teamrun.NewPGStore(), teamrun.NewPGCheckpointStore()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run, err := runs.GetTx(t.Context(), tx, "ws", runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoints.PutTx(t.Context(), tx, teamrun.WorkflowCheckpointV1{
		SchemaVersion: 1, Stamp: teamrun.WorkflowRunStamp{WorkspaceID: "ws", WorkflowID: run.WorkflowID,
			WorkflowVersion: run.WorkflowVersion, RunSnapshotID: run.RunSnapshotID},
		RunID: runID, TeamRunGeneration: run.Generation, ExecutionLeaseEpoch: run.ExecutionLeaseEpoch,
		NodeID: "confirm", CompletedOutputs: map[string]json.RawMessage{"draft": json.RawMessage(`"employee-private-draft"`)},
		WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.teamRunHumanTasks = &teamrun.HumanTaskReader{Pool: pool}
	server.teamRunHumanResume = &teamrun.HumanResumeService{Transactions: pool, Runs: runs, Checkpoints: checkpoints, Tasks: server.Tasks}
	var inputID, sessionID string
	if err := pool.QueryRow(t.Context(), `SELECT input_revision_id,workbench_session_id FROM weave_dispatch_input_revisions
		WHERE workspace_id='ws' AND user_id='user' AND consumed_run_id=$1`, runID).Scan(&inputID, &sessionID); err != nil {
		t.Fatal(err)
	}
	source := humanTaskSource{InputRevisionID: inputID, WorkbenchSessionID: sessionID, InteractionID: teamrun.HumanInteractionID(run)}
	call := func(method, userID, selectedRun string, refs humanTaskSource) *httptest.ResponseRecorder {
		t.Helper()
		query := url.Values{"input_revision_id": {refs.InputRevisionID}, "workbench_session_id": {refs.WorkbenchSessionID}, "interaction_id": {refs.InteractionID}}
		body, _ := json.Marshal(completeHumanTaskRequest{InputRevisionID: refs.InputRevisionID,
			WorkbenchSessionID: refs.WorkbenchSessionID, InteractionID: refs.InteractionID,
			Payload: json.RawMessage(`{"decision":"approved"}`), IdempotencyKey: "answer-one"})
		path := "/v1/human-tasks/" + selectedRun + "?" + query.Encode()
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(request, recorder)
		c.Set("tenant", "ws")
		c.Set("user_id", userID)
		// A developer role must not bypass another employee's Workbench input.
		c.Set("roles", []string{"developer"})
		c.SetParamNames("run_id")
		c.SetParamValues(selectedRun)
		handler := server.handleGetHumanTask
		if method == http.MethodPost {
			handler = server.handleCompleteHumanTask
		}
		if err := handler(c); err != nil {
			c.Echo().HTTPErrorHandler(err, c)
		}
		return recorder
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, test := range []struct {
			name, userID, runID string
			source              humanTaskSource
		}{
			{"other employee", "user-other", runID, source},
			{"other run", "user", "missing-run", source},
			{"other input", "user", runID, humanTaskSource{InputRevisionID: "another-input", WorkbenchSessionID: sessionID, InteractionID: source.InteractionID}},
			{"other session", "user", runID, humanTaskSource{InputRevisionID: inputID, WorkbenchSessionID: "another-session", InteractionID: source.InteractionID}},
			{"other interaction", "user", runID, humanTaskSource{InputRevisionID: inputID, WorkbenchSessionID: sessionID, InteractionID: "another-question"}},
		} {
			if response := call(method, test.userID, test.runID, test.source); response.Code != http.StatusNotFound {
				t.Fatalf("%s %s: status=%d body=%s", method, test.name, response.Code, response.Body.String())
			}
		}
	}
	if response := call(http.MethodGet, "user", runID, source); response.Code != http.StatusOK ||
		!bytes.Contains(response.Body.Bytes(), []byte("employee-private-draft")) || !bytes.Contains(response.Body.Bytes(), []byte(inputID)) {
		t.Fatalf("owner could not read exact native-inbox task: %d %s", response.Code, response.Body.String())
	}
	for range 2 {
		if response := call(http.MethodPost, "user", runID, source); response.Code != http.StatusAccepted {
			t.Fatalf("owner completion/replay failed: %d %s", response.Code, response.Body.String())
		}
	}
}
