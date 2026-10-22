package loomruntime

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/storeext"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

func TestAttemptLeaseBatchKeepsExactOwnerProofRealPG(t *testing.T) {
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	owner := AttemptLeaseOwner{WorkspaceID: "ws", RunID: "run", Generation: 1, AttemptID: uuid.New(), GraphName: "graph", RunStartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if _, err := pool.Exec(t.Context(), `INSERT INTO weave_run_attempt_leases(workspace_id,run_id,attempt_generation,attempt_id,graph_name,run_started_at,attempt_started_at,state,heartbeat_at,lease_expires_at,retry_count)
	 VALUES($1,$2,$3,$4,$5,$6,now(),'active',now(),now()+interval '120 seconds',0)`, owner.WorkspaceID, owner.RunID, owner.Generation, owner.AttemptID, owner.GraphName, owner.RunStartedAt); err != nil {
		t.Fatal(err)
	}
	records := storeext.New(pool)
	batcher, err := NewAttemptLeaseBatcher(records)
	if err != nil {
		t.Fatal(err)
	}
	defer batcher.Close()
	registry, err := NewExpectedRunRegistry(records)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithAttemptLeaseBatcher(t.Context(), batcher)
	wrongGraph, wrongStart, wrongGeneration := owner, owner, owner
	wrongGraph.GraphName = "different"
	wrongStart.RunStartedAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	wrongGeneration.Generation++
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, candidate := range []AttemptLeaseOwner{owner, wrongGraph, wrongStart, wrongGeneration} {
		wg.Add(1)
		go func(candidate AttemptLeaseOwner) {
			defer wg.Done()
			<-start
			renewal, err := registry.(AttemptLeaseHeartbeat).RenewAttempt(ctx, candidate, 120*time.Second)
			if candidate == owner {
				if err != nil || renewal.LeaseExpiresAt.Sub(renewal.HeartbeatAt) != 120*time.Second {
					t.Errorf("valid renewal: %+v %v", renewal, err)
				}
			} else if !errors.Is(err, ErrAttemptLeaseHeartbeatOwnerLost) {
				t.Errorf("a different owner received a renewal proof: %+v %v", candidate, err)
			}
		}(candidate)
	}
	close(start)
	wg.Wait()
	if _, err := pool.Exec(t.Context(), `UPDATE weave_run_attempt_leases SET state='closed' WHERE run_id=$1`, owner.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.(AttemptLeaseHeartbeat).RenewAttempt(ctx, owner, 120*time.Second); !errors.Is(err, ErrAttemptLeaseHeartbeatOwnerLost) {
		t.Fatalf("closed lease renewed: %v", err)
	}
}
