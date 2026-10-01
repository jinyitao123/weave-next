package api

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"testing"
)

func seedInputLineage(t *testing.T, pool *pgxpool.Pool, workspace, user, id, root, parent string, accepted bool) {
	t.Helper()
	kind := "initial"
	var parentRef, parentRun, parentDigest any
	materials := "[]"
	if parent != "" {
		kind = "revision"
		parentRef = parent
		parentRun = "parent-run"
		parentDigest = string(makeDigestForLineage())
		materials = `[{"id":"prior-material"}]`
	}
	var run, task, at any
	if accepted {
		run = "run-" + id
		task = "task-" + id
		at = "2026-10-01T01:00:00Z"
	}
	_, err := pool.Exec(t.Context(), `INSERT INTO weave_dispatch_input_revisions(workspace_id,user_id,workbench_session_id,input_revision_id,registration_id,registration_sha256,source_messages,task,task_sha256,team_id,mode,workflow_id,workflow_version,client_request_id,is_current,execution_task,revision_kind,root_input_revision_id,parent_input_revision_id,parent_run_id,parent_delivery_digest,parent_materials,consumed_run_id,consumed_task_id,consumed_at)
 VALUES($1,$2,'same-session',$3,$4,repeat('a',64),'["source"]','task',repeat('a',64),'team','workflow','flow',1,$5,false,'task',$6,$7,$8,$9,$10,$11::jsonb,$12,$13,$14)`, workspace, user, id, uuid.NewString(), uuid.NewString(), kind, root, parentRef, parentRun, parentDigest, materials, run, task, at)
	if err != nil {
		t.Fatal(err)
	}
}
func makeDigestForLineage() []byte {
	result := make([]byte, 64)
	for i := range result {
		result[i] = 'a'
	}
	return result
}
func TestInputSupersessionRequiresAcceptedExactOwnedWorkLineageRealPG(t *testing.T) {
	server, pool := newTeamDispatchTestServer(t)
	root, unrelated, child, grandchild := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	seedInputLineage(t, pool, "ws", "user", root, root, "", true)
	seedInputLineage(t, pool, "ws", "user", unrelated, unrelated, "", true)
	replacement, err := server.readInputReplacement(t.Context(), "ws", "user", root, root)
	if err != nil || replacement != "" {
		t.Fatalf("unrelated same-session work replaced original: %q %v", replacement, err)
	}
	seedInputLineage(t, pool, "ws", "user", child, root, root, false)
	replacement, err = server.readInputReplacement(t.Context(), "ws", "user", root, root)
	if err != nil || replacement != "" {
		t.Fatal("unaccepted registration incorrectly superseded original work")
	}
	if _, err := pool.Exec(t.Context(), `UPDATE weave_dispatch_input_revisions SET consumed_run_id=$2,consumed_task_id=$3,consumed_at=now() WHERE input_revision_id=$1`, child, "run-"+child, "task-"+child); err != nil {
		t.Fatal(err)
	}
	seedInputLineage(t, pool, "ws", "user", grandchild, root, child, true)
	replacement, err = server.readInputReplacement(t.Context(), "ws", "user", root, root)
	if err != nil || replacement != grandchild {
		t.Fatalf("accepted multigeneration chain missing: %q %v", replacement, err)
	}
	for _, scope := range []struct{ workspace, user string }{{"ws", "other-user"}, {"other-ws", "user"}} {
		replacement, err := server.readInputReplacement(t.Context(), scope.workspace, scope.user, root, root)
		if err != nil || replacement != "" {
			t.Fatalf("supersession crossed account/workspace: %q %v", replacement, err)
		}
	}
	otherRoot := uuid.NewString()
	replacement, err = server.readInputReplacement(t.Context(), "ws", "user", root, otherRoot)
	if err != nil || replacement != "" {
		t.Fatal("supersession ignored accurate root")
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE weave_dispatch_input_revisions RENAME TO unavailable_input_lineage`); err != nil {
		t.Fatal(err)
	}
	if _, err := server.readInputReplacement(t.Context(), "ws", "user", root, root); err == nil {
		t.Fatal("reading failure was turned into a guessed current/superseded state")
	}
}
