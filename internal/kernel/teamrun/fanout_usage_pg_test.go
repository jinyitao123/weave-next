package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/testutil"
	"github.com/jinyitao123/weave/internal/kernel/fanout"
	"github.com/jinyitao123/weave/internal/kernel/loomruntime"
)

func fanoutTestStepUsage(input, output int) fanoutLegStepUsageV1 {
	return fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{{
		AttemptID: "attempt", Usage: contract.Usage{InputTokens: input, OutputTokens: output}, HasTokens: true, HasCost: true,
	}}}
}

// The parent counts what every finished branch step spent, once, on top of
// what it spent itself, and stays usage-incomplete for anything it cannot
// count instead of treating it as free.
func TestFanoutJoinAddsBranchUsageToParentRealPG(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	runtime := &WorkflowSerialRuntime{Transactions: pool}
	record := func(group, legID, nodeID string, ordinal int, usage fanoutLegStepUsageV1) {
		t.Helper()
		usage.Attempts[0].AttemptID = group + "/" + legID + "/" + nodeID
		leg := FanoutLegTaskPayloadV1{GroupID: group, LegID: legID}
		if err := runtime.recordFanoutLegStep(ctx, "ws", leg, nodeID, ordinal, json.RawMessage(`"ok"`), nil, usage, nil); err != nil {
			t.Fatal(err)
		}
	}
	parentUsage := func() WorkflowCheckpointV1 {
		accumulator := loomruntime.NewUsageAccumulator()
		callID, _ := accumulator.NextCall("parent", "lead")
		if err := confirmNodeUsageAttempts(&accumulator, callID, fanoutTestStepUsage(5, 1).Attempts); err != nil {
			t.Fatal(err)
		}
		encoded, err := accumulator.MarshalCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		return WorkflowCheckpointV1{RunID: "parent", Usage: encoded}
	}
	merge := func(checkpoint *WorkflowCheckpointV1, join fanout.JoinResultV1) loomruntime.UsageTotals {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := mergeFanoutLegUsageTx(ctx, tx, "ws", checkpoint, join); err != nil {
			t.Fatal(err)
		}
		accumulator, err := loomruntime.UnmarshalUsageAccumulator(checkpoint.Usage)
		if err != nil {
			t.Fatal(err)
		}
		return accumulator.Totals()
	}
	succeeded := func(legID string) fanout.JoinResultLegV1 {
		return fanout.JoinResultLegV1{LegID: legID, BranchID: legID, DecisionState: fanout.LegDecisionSucceeded}
	}

	// Two legs, one with two steps: every step counts once, plus the parent.
	record("all", "a", "draft", 0, fanoutTestStepUsage(100, 10))
	record("all", "a", "check", 1, fanoutTestStepUsage(200, 20))
	record("all", "b", "review", 0, fanoutTestStepUsage(400, 40))
	checkpoint := parentUsage()
	totals := merge(&checkpoint, fanout.JoinResultV1{GroupID: "all", Legs: []fanout.JoinResultLegV1{succeeded("a"), succeeded("b")}})
	if totals.InputTokens != 705 || totals.OutputTokens != 71 || checkpoint.UsageIncompleteReason != "" {
		t.Fatalf("joined usage = %+v reason=%q, want 705/71 and complete", totals, checkpoint.UsageIncompleteReason)
	}

	// A failed leg keeps what its finished step spent, but the step that
	// failed left no receipt, so the parent is usage-incomplete.
	record("failed", "a", "draft", 0, fanoutTestStepUsage(100, 10))
	record("failed", "b", "draft", 0, fanoutTestStepUsage(300, 30))
	checkpoint = parentUsage()
	totals = merge(&checkpoint, fanout.JoinResultV1{GroupID: "failed", Legs: []fanout.JoinResultLegV1{succeeded("a"),
		{LegID: "b", BranchID: "b", DecisionState: fanout.LegDecisionFailed}}})
	if totals.InputTokens != 405 || checkpoint.UsageComplete || checkpoint.UsageIncompleteReason != UsageIncompleteReasonParallelLegs {
		t.Fatalf("failed-leg usage = %+v complete=%v reason=%q", totals, checkpoint.UsageComplete, checkpoint.UsageIncompleteReason)
	}

	// A step finished before steps stored their usage is unknown, not zero.
	if _, err := pool.Exec(ctx, `INSERT INTO weave_fanout_leg_steps(workspace_id,group_id,leg_id,node_id,step_ordinal,output,artifact_task_ids)
		VALUES('ws','legacy','a','draft',0,'"ok"','[]')`); err != nil {
		t.Fatal(err)
	}
	checkpoint = parentUsage()
	totals = merge(&checkpoint, fanout.JoinResultV1{GroupID: "legacy", Legs: []fanout.JoinResultLegV1{succeeded("a")}})
	if totals.InputTokens != 5 || checkpoint.UsageIncompleteReason != UsageIncompleteReasonParallelLegs {
		t.Fatalf("legacy step usage = %+v reason=%q, want parent only and incomplete", totals, checkpoint.UsageIncompleteReason)
	}

	// A step that ran again after an unfinished attempt keeps its reason.
	rerun := fanoutTestStepUsage(50, 5)
	rerun.IncompleteReason = UsageIncompleteReasonLegRerun
	record("rerun", "a", "draft", 0, rerun)
	checkpoint = parentUsage()
	totals = merge(&checkpoint, fanout.JoinResultV1{GroupID: "rerun", Legs: []fanout.JoinResultLegV1{succeeded("a")}})
	if totals.InputTokens != 55 || checkpoint.UsageIncompleteReason != UsageIncompleteReasonLegRerun {
		t.Fatalf("rerun step usage = %+v reason=%q", totals, checkpoint.UsageIncompleteReason)
	}
}
