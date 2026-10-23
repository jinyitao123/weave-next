package capabilities

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

// The history-scanning admission queries that the live-owner queries replaced.
// They stay here as the reference the replacements must agree with.
const legacyActiveCapabilityInvocationsSQL = `SELECT
 (SELECT count(*) FROM weave_capability_invocations i
  JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id AND q.capability_invocation_id=i.invocation_id
  WHERE i.workspace_id=$1
    AND (q.status IN ('queued','running','cancel_requested') OR (q.status='completed' AND i.status='waiting') OR (q.status='failed' AND q.worker_id IS NOT NULL))
    AND NOT (i.application_id=$2 AND i.request_id=$3))
 +
 (SELECT count(*) FROM weave_capability_workflow_invocations i
  LEFT JOIN weave_workflow_admission_requests a ON a.workspace_id=i.workspace_id AND a.request_id=i.admission_request_id
  LEFT JOIN weave_kernel_publication_requests k ON k.workspace_id=i.workspace_id AND k.request_id=i.admission_request_id
  LEFT JOIN weave_team_runs r ON r.workspace_id=i.workspace_id AND r.run_id=i.run_id
  LEFT JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id
  WHERE i.workspace_id=$1 AND NOT (i.application_id=$2 AND i.request_id=$3)
    AND k.operation IS DISTINCT FROM 'published_closed'
    AND ((a.receipt IS NULL AND q.id IS NULL AND r.run_id IS NULL)
      OR r.status IN ('queued','running','parked','cancel_requested')
      OR (r.run_id IS NULL AND q.status IN ('queued','dispatched','running','cancel_requested'))))`

const legacyWorkflowLaneActiveSQL = `SELECT EXISTS(SELECT 1 FROM weave_capability_workflow_invocations i
  LEFT JOIN weave_workflow_admission_requests a ON a.workspace_id=i.workspace_id AND a.request_id=i.admission_request_id
  LEFT JOIN weave_kernel_publication_requests k ON k.workspace_id=i.workspace_id AND k.request_id=i.admission_request_id
  LEFT JOIN weave_team_runs r ON r.workspace_id=i.workspace_id AND r.run_id=i.run_id
  LEFT JOIN weave_task_queue q ON q.workspace_id=i.workspace_id AND q.id=i.task_id
  WHERE i.workspace_id=$1 AND i.application_id=$2 AND i.lane=$3 AND i.invocation_id<>$4
    AND k.operation IS DISTINCT FROM 'published_closed'
    AND ((a.receipt IS NULL AND q.id IS NULL AND r.run_id IS NULL)
      OR COALESCE(r.status, CASE q.status WHEN 'queued' THEN 'queued' WHEN 'dispatched' THEN 'queued'
        WHEN 'running' THEN 'running' WHEN 'cancel_requested' THEN 'cancel_requested' ELSE q.status END)
        IN ('queued','running','parked','cancel_requested')))`

// TestLiveAdmissionQueriesMatchLegacyRealPG checks the live-owner quota count
// and lane check against the legacy queries over random owner states. The
// tables carry only the queried columns and the production keys; every
// workflow invocation has its admission request, as the foreign key requires,
// and only capability tasks carry an invocation, as the task identity check
// requires.
func TestLiveAdmissionQueriesMatchLegacyRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
	 CREATE TABLE weave_task_queue(id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, status TEXT NOT NULL,
	  worker_id TEXT, capability_invocation_id TEXT, identity_kind TEXT NOT NULL DEFAULT 'team_workflow',
	  UNIQUE(workspace_id,id), CHECK((capability_invocation_id IS NOT NULL) = (identity_kind='capability')));
	 CREATE TABLE weave_team_runs(workspace_id TEXT, run_id TEXT, status TEXT NOT NULL, PRIMARY KEY(workspace_id,run_id));
	 CREATE TABLE weave_capability_invocations(workspace_id TEXT, application_id TEXT, request_id TEXT,
	  invocation_id TEXT NOT NULL, task_id TEXT, status TEXT NOT NULL,
	  PRIMARY KEY(workspace_id,application_id,request_id), UNIQUE(workspace_id,invocation_id));
	 CREATE TABLE weave_workflow_admission_requests(workspace_id TEXT, request_id TEXT, receipt JSONB,
	  PRIMARY KEY(workspace_id,request_id));
	 CREATE TABLE weave_kernel_publication_requests(workspace_id TEXT, request_id TEXT, operation TEXT NOT NULL,
	  PRIMARY KEY(workspace_id,request_id));
	 CREATE TABLE weave_capability_workflow_invocations(workspace_id TEXT, application_id TEXT, invocation_id TEXT,
	  request_id TEXT NOT NULL, admission_request_id TEXT NOT NULL, run_id TEXT, task_id TEXT, lane TEXT,
	  PRIMARY KEY(workspace_id,application_id,invocation_id), UNIQUE(workspace_id,application_id,request_id),
	  UNIQUE(workspace_id,admission_request_id), UNIQUE(workspace_id,run_id),
	  FOREIGN KEY(workspace_id,admission_request_id) REFERENCES weave_workflow_admission_requests(workspace_id,request_id))`); err != nil {
		t.Fatal(err)
	}
	workspaces := []string{"w1", "w2"}
	apps := []string{"a1", "a2"}
	lanes := []string{"", "L1", "L2"}
	taskStatuses := []string{"queued", "dispatched", "running", "cancel_requested", "completed", "failed", "cancelled"}
	runStatuses := []string{"queued", "running", "parked", "cancel_requested", "completed", "failed", "cancelled"}
	inlineStatuses := []string{"queued", "running", "waiting", "completed", "failed"}
	operations := []string{"published_closed", "published_open", "admitted"}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	rng := rand.New(rand.NewSource(20261022))
	pick := func(values []string) string { return values[rng.Intn(len(values))] }
	var compared, nonzero, active int
	// A failed round must still release its connection: cleanups run in reverse,
	// so this rollback precedes the pool's Close, which waits for connections.
	var tx pgx.Tx
	t.Cleanup(func() {
		if tx != nil {
			_ = tx.Rollback(context.Background())
		}
	})
	for round := 0; round < 400; round++ {
		var err error
		tx, err = pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		exec := func(sql string, args ...any) {
			t.Helper()
			if _, err := tx.Exec(ctx, sql, args...); err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
		}
		taskIDs := map[string][]string{}
		for _, ws := range workspaces {
			for k := 0; k < rng.Intn(7); k++ {
				id := fmt.Sprintf("%s-t%d", ws, k)
				worker := ""
				if rng.Intn(2) == 0 {
					worker = "worker"
				}
				exec(`INSERT INTO weave_task_queue(id,workspace_id,status,worker_id) VALUES($1,$2,$3,$4)`, id, ws, pick(taskStatuses), nullable(worker))
				taskIDs[ws] = append(taskIDs[ws], id)
			}
		}
		// A task reference is null, a task of the workspace, a task of the other
		// workspace, or a task that does not exist.
		taskRef := func(ws string) string {
			switch n := rng.Intn(10); {
			case n == 0:
				return ""
			case n == 1:
				return "missing-task"
			case n == 2 && len(taskIDs["w1"])+len(taskIDs["w2"]) > 0:
				all := append(append([]string{}, taskIDs["w1"]...), taskIDs["w2"]...)
				return pick(all)
			case len(taskIDs[ws]) > 0:
				return pick(taskIDs[ws])
			}
			return "missing-task"
		}
		workflowInvocations := map[string][]string{}
		for _, ws := range workspaces {
			for k := 0; k < rng.Intn(5); k++ {
				invocationID, taskID := fmt.Sprintf("%s-ci%d", ws, k), taskRef(ws)
				exec(`INSERT INTO weave_capability_invocations(workspace_id,application_id,request_id,invocation_id,task_id,status)
				 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, ws, pick(apps), fmt.Sprintf("r%d", rng.Intn(4)), invocationID, nullable(taskID), pick(inlineStatuses))
				if taskID != "" && rng.Intn(4) != 0 {
					owner := invocationID
					if rng.Intn(5) == 0 {
						owner = "another-invocation"
					}
					exec(`UPDATE weave_task_queue SET capability_invocation_id=$2,identity_kind='capability' WHERE id=$1`, taskID, owner)
				}
			}
			for k := 0; k < rng.Intn(9); k++ {
				invocationID := fmt.Sprintf("%s-wi%d", ws, k)
				admissionID := fmt.Sprintf("%s-adm%d", ws, k)
				var receipt any
				if rng.Intn(2) == 0 {
					receipt = "{}"
				}
				exec(`INSERT INTO weave_workflow_admission_requests(workspace_id,request_id,receipt) VALUES($1,$2,$3::jsonb)`, ws, admissionID, receipt)
				if rng.Intn(5) < 2 {
					exec(`INSERT INTO weave_kernel_publication_requests(workspace_id,request_id,operation) VALUES($1,$2,$3)`, ws, admissionID, pick(operations))
				}
				runID := ""
				if rng.Intn(5) != 0 {
					runID = fmt.Sprintf("%s-run%d", ws, k)
					if rng.Intn(5) < 3 {
						exec(`INSERT INTO weave_team_runs(workspace_id,run_id,status) VALUES($1,$2,$3)`, ws, runID, pick(runStatuses))
					}
				}
				exec(`INSERT INTO weave_capability_workflow_invocations(workspace_id,application_id,invocation_id,request_id,admission_request_id,run_id,task_id,lane)
				 VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT DO NOTHING`, ws, pick(apps), invocationID, fmt.Sprintf("r%d", rng.Intn(4)), admissionID,
					nullable(runID), nullable(taskRef(ws)), nullable(pick(lanes)))
				workflowInvocations[ws] = append(workflowInvocations[ws], invocationID)
			}
		}
		for _, ws := range workspaces {
			for _, app := range apps {
				for _, request := range []string{"r0", "r1", "r2", "r3", "unused"} {
					var want int
					if err := tx.QueryRow(ctx, legacyActiveCapabilityInvocationsSQL, ws, app, request).Scan(&want); err != nil {
						t.Fatal(err)
					}
					got, err := countActiveCapabilityInvocations(ctx, tx, ws, app, request)
					if err != nil {
						t.Fatal(err)
					}
					if got != want {
						t.Fatalf("round %d: active count for %s/%s/%s = %d, legacy %d", round, ws, app, request, got, want)
					}
					compared++
					if want > 0 {
						nonzero++
					}
				}
				for _, lane := range lanes[1:] {
					for _, invocationID := range append(workflowInvocations[ws], "unused") {
						var want bool
						if err := tx.QueryRow(ctx, legacyWorkflowLaneActiveSQL, ws, app, lane, invocationID).Scan(&want); err != nil {
							t.Fatal(err)
						}
						got, err := workflowLaneActive(ctx, tx, ws, app, lane, invocationID)
						if err != nil {
							t.Fatal(err)
						}
						if got != want {
							t.Fatalf("round %d: lane %s/%s/%s excluding %s active = %v, legacy %v", round, ws, app, lane, invocationID, got, want)
						}
						compared++
						if want {
							active++
						}
					}
				}
			}
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Guard against a generator that only ever produces empty answers.
	if nonzero < compared/10 || active == 0 {
		t.Fatalf("random states too sparse: %d comparisons, %d nonzero counts, %d active lanes", compared, nonzero, active)
	}
}
