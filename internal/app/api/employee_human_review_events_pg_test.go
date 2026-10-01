package api

import (
	"encoding/json"
	"testing"
)

// Decision 002 / C34: a human wait in a Workbench run reaches the employee who
// started the work only through the native inbox, once per interaction.
func TestHumanWaitBecomesOneHumanReviewEventForTheInitiatorRealPG(t *testing.T) {
	pool, runID := succeededRunForOutboxTest(t)
	detail := `{"schema_version":1,"wait_type":"human","node_id":"confirm","success_node_id":"deliver","resume_schema":{"type":"object"},"task":{"title":"确认合同条款","instructions":"请核对付款条款后继续"}}`
	if _, err := pool.Exec(t.Context(), `UPDATE weave_team_runs SET status='parked',terminal_at=NULL,wait_kind='human',wait_detail=$2::jsonb,
		resume_token_hash='\x01'::bytea,checkpoint_ref='checkpoint-1',updated_at=statement_timestamp()
		WHERE workspace_id='ws' AND run_id=$1`, runID, detail); err != nil {
		t.Fatalf("park run for human review: %v", err)
	}
	worker := &employeeRunEventWorker{Pool: pool}
	for range 2 {
		if err := worker.materialize(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(t.Context(), `SELECT event_scope,payload FROM weave_employee_run_event_outbox WHERE workspace_id='ws' AND run_id=$1`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []map[string]any
	var scopes []string
	for rows.Next() {
		var scope string
		var raw []byte
		if err := rows.Scan(&scope, &raw); err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		scopes, events = append(scopes, scope), append(events, payload)
	}
	if len(events) != 1 {
		t.Fatalf("parked human wait produced %d events (scopes %v), want exactly one", len(events), scopes)
	}
	event := events[0]
	source, _ := event["source"].(map[string]any)
	interaction, _ := source["interactionReference"].(string)
	if event["kind"] != "human_review" || event["assigneeAccountId"] != "forge-user" || interaction == "" || scopes[0] != interaction ||
		source["runReference"] != runID || source["idempotencyKey"] != "weave-team-run-human:"+interaction {
		t.Fatalf("unexpected human review event: scope=%s %v", scopes[0], event)
	}
	if summary, _ := event["summary"].(string); summary != "确认合同条款：请核对付款条款后继续" {
		t.Fatalf("summary = %q", summary)
	}
}
