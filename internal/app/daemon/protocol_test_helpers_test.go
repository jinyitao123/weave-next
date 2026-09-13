package daemon

import (
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/runtimebridge"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func testExecutionClaim(t *testing.T, task *taskqueue.Task) *runtimeprotocol.ExecutionClaim {
	t.Helper()
	now := time.Now().UTC()
	copy := *task
	copy.Kind = "engine_exec"
	copy.Status = taskqueue.StatusRunning
	copy.WorkerID = "runtime:test"
	if copy.ClaimEpoch < 1 {
		copy.ClaimEpoch = 1
	}
	if copy.UpdatedAt.IsZero() {
		copy.UpdatedAt = now
	}
	if copy.LeaseExpiresAt == nil {
		expires := now.Add(time.Minute)
		copy.LeaseExpiresAt = &expires
	}
	claim, err := runtimebridge.Claim(&copy, copy.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}
