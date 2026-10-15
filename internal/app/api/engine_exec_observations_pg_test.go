package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

// A member that serves two workflow steps must only get back the CLI events
// of the step being compensated, never the other step's.
func TestEngineExecObservationsStayWithTheirNodeRealPG(t *testing.T) {
	fixture := newDevelopmentTrialOutputFixture(t, 1)
	member := fixture.workers[0].ID
	insert := func(id, payload, tool string, minute int) {
		t.Helper()
		result := fmt.Sprintf(`{"events":[{"type":"tool_use","tool":%q}]}`, tool)
		if _, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO weave_task_queue(id,workspace_id,agent,agent_id,agent_version,source,status,kind,identity_kind,identity_schema_version,execution_scope,run_snapshot_id,payload,result,completed_at,actor_subject)
			VALUES($1,$2,'member',$3,1,'api','completed','engine_exec','agent',2,'team_free_collab',$4,$5::jsonb,$6::jsonb,$7,$8::jsonb)`,
			id, fixture.workspaceID, member, fixture.runID, payload, result, fixture.now.Add(time.Duration(minute)*time.Minute),
			fmt.Sprintf(`{"workspace_id":%q,"user_id":%q}`, fixture.workspaceID, fixture.actorID)); err != nil {
			t.Fatal(err)
		}
	}
	insert("draft-step", `{"node_id":"draft"}`, "write_draft", 1)
	insert("review-step", `{"node_id":"review"}`, "check_terms", 2)
	insert("unscoped", `{}`, "unknown_step", 3)

	store := taskqueue.New(fixture.pool, nil, time.Minute)
	for node, want := range map[string]string{"draft": "draft-step", "review": "review-step"} {
		observations, err := store.ListEngineExecObservations(fixture.ctx, fixture.workspaceID, fixture.runID, node, member)
		if err != nil {
			t.Fatal(err)
		}
		if len(observations) != 1 || observations[0].TaskID != want {
			t.Fatalf("node %s observations = %+v, want only %s", node, observations, want)
		}
	}
	if _, err := store.ListEngineExecObservations(fixture.ctx, fixture.workspaceID, fixture.runID, "", member); err == nil {
		t.Fatal("an observation query without a node must be refused")
	}
}
