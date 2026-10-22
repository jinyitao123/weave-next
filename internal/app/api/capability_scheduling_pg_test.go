package api

import (
	"errors"
	"net/http"
	"testing"

	"github.com/jinyitao123/weave/internal/app/capabilities"
)

func TestCapabilitySchedulingIsAuthorizedFrozenAndInheritedRealPG(t *testing.T) {
	f := newCapabilityWorkflowContractFixture(t)
	f.publishCapability(t, "scheduled")
	app, key := f.application(t, "scheduled caller")
	f.grant(t, app.ID, "scheduled", true)
	path := "/v1/capabilities/scheduled/versions/1/invocations"
	body := map[string]any{"request_id": "priority-once", "priority": "interactive", "fairness_key": "faction-a", "input": map[string]any{"document": "bounded request"}}
	if result := f.call(t, key, http.MethodPost, path, body); result.status != http.StatusBadRequest {
		t.Fatalf("priority ceiling was ignored: %d %s", result.status, result.body["error"])
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM weave_capability_workflow_invocations WHERE application_id=$1`, app.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("denied priority left an invocation: %d %v", count, err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO weave_users(id,tenant_id,username,password,role) VALUES('developer-priority','ws','developer-priority','unused','developer')`); err != nil {
		t.Fatal(err)
	}
	if err := f.access.SetApplicationMaxPriority(t.Context(), "ws", "developer-priority", app.ID, "interactive"); !errors.Is(err, capabilities.ErrAccessDenied) {
		t.Fatalf("developer raised an application's ceiling: %v", err)
	}
	if err := f.access.SetApplicationMaxPriority(t.Context(), "ws", "user", app.ID, "interactive"); err != nil {
		t.Fatal(err)
	}
	accepted := f.call(t, key, http.MethodPost, path, body)
	if accepted.status != http.StatusAccepted {
		t.Fatalf("authorized priority rejected: %d %s", accepted.status, accepted.body["error"])
	}
	var priority int
	var fair string
	if err := f.pool.QueryRow(t.Context(), `SELECT q.priority,q.fairness_key FROM weave_task_queue q JOIN weave_capability_workflow_invocations i ON i.task_id=q.id WHERE i.application_id=$1 AND i.request_id='priority-once'`, app.ID).Scan(&priority, &fair); err != nil || priority != 1 || fair != app.ID+":faction-a" {
		t.Fatalf("frozen scheduling did not reach the execution queue: %d %q %v", priority, fair, err)
	}
	if replay := f.call(t, key, http.MethodPost, path, body); replay.status != http.StatusAccepted || !replay.boolField(t, "replayed") {
		t.Fatalf("exact replay failed: %d", replay.status)
	}
	body["priority"] = "normal"
	if changed := f.call(t, key, http.MethodPost, path, body); changed.status != http.StatusConflict {
		t.Fatalf("priority was changed under the same request: %d", changed.status)
	}
	body["priority"], body["fairness_key"] = "interactive", "faction-b"
	if changed := f.call(t, key, http.MethodPost, path, body); changed.status != http.StatusConflict {
		t.Fatalf("fairness was changed under the same request: %d", changed.status)
	}
}
