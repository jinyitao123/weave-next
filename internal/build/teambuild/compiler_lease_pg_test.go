package teambuild

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestRenewOperationStepRejectsExpiredLeaseRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	store := New(pool, authorizeFixedClock{now: now})
	run, err := store.CreateBuildRun(ctx, "workspace-1", "compiler-lease", CreateRunParams{
		Brief: authorizeTestBrief(ModeCreate), Contract: authorizeTestContract(),
		ExpiresAt: now.Add(time.Hour), CreatedBy: "lease-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const operationID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_build_blueprint_revisions
		(workspace_id,build_run_id,revision_no,blueprint_json,blueprint_hash,change_set_json,change_set_hash,baseline_hash,workflow_mode,evaluation_contract_hash,created_at)
		VALUES ($1,$2,1,'{}',$3,'{}',$3,$3,'template',$3,$4)`, run.WorkspaceID, run.BuildRunID, hash, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO weave_team_build_operation_steps
		(workspace_id,build_run_id,revision_no,operation_id,operation_index,operation_type,status,depends_on,input_hash,lease_owner,lease_epoch,lease_until,attempt,created_at,updated_at,started_at)
		VALUES ($1,$2,1,$3,0,'agent_create','running','[]',$4,'worker-1',1,$5,1,$6,$6,$6)`,
		run.WorkspaceID, run.BuildRunID, operationID, hash, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenewOperationStepLease(ctx, run.WorkspaceID, run.BuildRunID, 1, operationID, "worker-1", 1, time.Minute); err != nil {
		t.Fatalf("renew live lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE weave_team_build_operation_steps SET lease_until=$4,updated_at=$5
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=1 AND operation_id=$3`, run.WorkspaceID, run.BuildRunID, operationID, now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RenewOperationStepLease(ctx, run.WorkspaceID, run.BuildRunID, 1, operationID, "worker-1", 1, time.Minute); !errors.Is(err, ErrOperationStepLeaseLost) {
		t.Fatalf("expired operation lease renewed: %v", err)
	}
}
