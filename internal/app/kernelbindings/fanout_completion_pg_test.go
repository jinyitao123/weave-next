package kernelbindings

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

type completionEnqueuer struct{ fail bool }

func (e completionEnqueuer) EnqueueTx(ctx context.Context, tx pgx.Tx, task *taskqueue.Task) error {
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO completion_attempts(id, payload) VALUES ($1,$2::jsonb)`, task.ID, string(raw))
	if err != nil {
		return err
	}
	if e.fail {
		return errors.New("injected enqueue failure")
	}
	return nil
}

func fanoutCompletionFixture(t *testing.T) (*pgxpool.Pool, fanout.Group) {
	t.Helper()
	pool := productContextFixture(t)
	_, err := pool.Exec(context.Background(), `
 CREATE TABLE weave_agents(id text, workspace_id text, name text, version int, deleted bool);
 CREATE TABLE weave_teams(id text, workspace_id text, lead_avatar_id text, status text);
 CREATE TABLE weave_team_run_snapshots(run_id text, workspace_id text, lead_avatar_id text, lead_avatar_version int);
 CREATE TABLE weave_run_terminal_markers(workspace_id text, task_group_id text, run_snapshot_id text, phase text);
 CREATE TABLE weave_task_queue(id text, workspace_id text, task_group_id text, agent text, status text, result jsonb, error text, created_at timestamptz, updated_at timestamptz, completed_at timestamptz);
 CREATE TABLE completion_attempts(id text PRIMARY KEY, payload jsonb);
 INSERT INTO weave_agents VALUES ('current-lead','a','lead',7,false),('foreign','b','foreign-lead',99,false),('removed','a','removed',1,true);
 INSERT INTO weave_teams VALUES ('team','a','current-lead','active'),('foreign-team','b','foreign','active');
 `)
	if err != nil {
		t.Fatal(err)
	}
	group, err := NewFanout(pool, nil).CreateGroup(context.Background(), fanout.Group{ID: "group", WorkspaceID: "a", AvatarAgent: "lead", UserID: "alice", OriginalRequest: "Summarize results"})
	if err != nil {
		t.Fatal(err)
	}
	return pool, group
}

func TestFanoutCompletionPreservesFrozenLeadDespiteDirectoryRename(t *testing.T) {
	pool, group := fanoutCompletionFixture(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
 INSERT INTO weave_team_run_snapshots VALUES ('source','a','frozen-lead',2),('foreign-source','b','private-lead',90);
 INSERT INTO weave_run_terminal_markers VALUES ('a','group','source','final'),('a','group','source','final'),('b','group','foreign-source','final');
 ALTER TABLE weave_agents RENAME TO product_agent_directory;
 ALTER TABLE weave_teams RENAME TO product_team_directory;
 `)
	if err != nil {
		t.Fatal(err)
	}
	// This plain Kernel store intentionally has no product directory port.
	reconciler := fanout.NewReconciler(fanout.New(pool, nil), completionEnqueuer{}, nil)
	for range 2 {
		if err := reconciler.ReconcileGroup(ctx, group.WorkspaceID, group.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	task := readCompletionTask(t, pool)
	if task.AgentID != "frozen-lead" || task.AgentVersion != 2 || task.ExecutionScope != execution.ScopeTeamFreeCollab || task.Subject != (execution.Subject{WorkspaceID: "a", UserID: "alice"}) {
		t.Fatalf("frozen completion identity changed: %+v", task)
	}
	assertCompletionState(t, pool, fanout.StatusResolved, 1)
}

func TestFanoutCompletionRejectsConflictingOrIncompleteFrozenIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, snapshots string
		want            error
	}{
		{"conflicting", `('first','a','lead-one',1),('second','a','lead-two',2)`, fanout.ErrCompletionLeadAmbiguous},
		{"incomplete", `('first','a','lead-one',NULL),('second','a','lead-one',NULL)`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, group := fanoutCompletionFixture(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `INSERT INTO weave_team_run_snapshots VALUES `+tc.snapshots+`; INSERT INTO weave_run_terminal_markers VALUES ('a','group','first','final'),('a','group','second','final')`); err != nil {
				t.Fatal(err)
			}
			err := fanout.NewReconciler(NewFanout(pool, nil), completionEnqueuer{}, nil).ReconcileGroup(ctx, group.WorkspaceID, group.ID, true)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("invalid frozen identity fell back to directory: %v", err)
			}
			assertCompletionState(t, pool, fanout.StatusResolving, 0)
		})
	}
}

func TestFanoutCompletionDirectoryFallbackAndEnqueueAreTransactional(t *testing.T) {
	pool, group := fanoutCompletionFixture(t)
	ctx := context.Background()
	unbound := fanout.NewReconciler(fanout.New(pool, nil), completionEnqueuer{}, nil)
	if err := unbound.ReconcileGroup(ctx, group.WorkspaceID, group.ID, true); !errors.Is(err, fanout.ErrCompletionLeadUnavailable) {
		t.Fatalf("unbound directory accepted: %v", err)
	}
	assertCompletionState(t, pool, fanout.StatusResolving, 0)
	failing := fanout.NewReconciler(NewFanout(pool, nil), completionEnqueuer{fail: true}, nil)
	if err := failing.ReconcileGroup(ctx, group.WorkspaceID, group.ID, true); err == nil {
		t.Fatal("enqueue failure was hidden")
	}
	assertCompletionState(t, pool, fanout.StatusResolving, 0)
	if err := fanout.NewReconciler(NewFanout(pool, nil), completionEnqueuer{}, nil).ReconcileGroup(ctx, group.WorkspaceID, group.ID, true); err != nil {
		t.Fatal(err)
	}
	task := readCompletionTask(t, pool)
	if task.AgentID != "current-lead" || task.AgentVersion != 7 || task.ExecutionScope != execution.ScopeTeamFreeCollab {
		t.Fatalf("directory fallback: %+v", task)
	}
	assertCompletionState(t, pool, fanout.StatusResolved, 1)
}

func TestCompletionLeadDirectoryUsesWorkspaceAndCallerTransaction(t *testing.T) {
	pool, _ := fanoutCompletionFixture(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE weave_agents SET version=8 WHERE id='current-lead'; UPDATE weave_teams SET status='paused' WHERE id='team'`); err != nil {
		t.Fatal(err)
	}
	lead, err := CompletionLead(ctx, tx, "a", "lead")
	if err != nil || lead.AgentVersion != 8 || lead.TeamFreeCollab {
		t.Fatalf("transaction-local fallback: %+v %v", lead, err)
	}
	for _, name := range []string{"foreign-lead", "removed"} {
		if _, err := CompletionLead(ctx, tx, "a", name); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("foreign or removed lead accepted: %s %v", name, err)
		}
	}
}

func readCompletionTask(t *testing.T, pool *pgxpool.Pool) taskqueue.Task {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), `SELECT payload FROM completion_attempts`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var task taskqueue.Task
	if err := json.Unmarshal(raw, &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func assertCompletionState(t *testing.T, pool *pgxpool.Pool, wantStatus string, wantAttempts int) {
	t.Helper()
	var status string
	var attempts int
	if err := pool.QueryRow(context.Background(), `SELECT status,(SELECT count(*) FROM completion_attempts) FROM weave_task_group WHERE id='group'`).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || attempts != wantAttempts {
		t.Fatalf("completion state: status=%s attempts=%d, want %s/%d", status, attempts, wantStatus, wantAttempts)
	}
}
