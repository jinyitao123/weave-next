package teamrun

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/db"
	"github.com/jinyitao123/weave/internal/base/deliverable"
	"github.com/jinyitao123/weave/internal/base/testutil"
)

// A finished branch step is kept once: a leg that runs again after a failure
// reads the first record back and does not overwrite it.
func TestFinishedBranchStepsAreKeptOncePerLeg(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	runtime := &WorkflowSerialRuntime{Transactions: pool}
	leg := FanoutLegTaskPayloadV1{GroupID: "group", LegID: "leg"}
	if err := runtime.recordFanoutLegStep(ctx, "ws", leg, "draft", 0, json.RawMessage(`"初稿"`), []string{"member:draft-run"}, fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := runtime.recordFanoutLegStep(ctx, "ws", leg, "draft", 0, json.RawMessage(`"另一份初稿"`), nil, fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{}}, nil); err != nil {
		t.Fatal(err)
	}
	other := FanoutLegTaskPayloadV1{GroupID: "group", LegID: "other-leg"}
	if err := runtime.recordFanoutLegStep(ctx, "ws", other, "check", 0, json.RawMessage(`"核对"`), []string{}, fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{}}, nil); err != nil {
		t.Fatal(err)
	}
	finished, err := runtime.finishedFanoutLegSteps(ctx, "ws", leg)
	if err != nil {
		t.Fatal(err)
	}
	draft, present := finished["draft"]
	if len(finished) != 1 || !present || string(draft.Output) != `"初稿"` || len(draft.ArtifactTaskIDs) != 1 || draft.ArtifactTaskIDs[0] != "member:draft-run" {
		t.Fatalf("finished steps = %+v", finished)
	}
}

type branchOutputRecorder struct {
	fail     error
	recorded []deliverable.WorkflowOutput
}

func (r *branchOutputRecorder) RecordWorkflowOutput(context.Context, deliverable.WorkflowOutput) error {
	return errors.New("outside the branch step's transaction")
}

func (r *branchOutputRecorder) RecordWorkflowOutputsTx(ctx context.Context, tx pgx.Tx, outputs []deliverable.WorkflowOutput) error {
	if r.fail != nil {
		return r.fail
	}
	var visible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM weave_fanout_leg_steps WHERE workspace_id='ws' AND node_id=$1)`, outputs[0].NodeID).Scan(&visible); err != nil || !visible {
		return errors.New("outputs recorded outside the step's transaction")
	}
	r.recorded = append(r.recorded, outputs...)
	return nil
}

// A branch step's outputs commit in the step's own transaction: recorded
// with the step, and neither kept when recording them fails.
func TestBranchStepOutputsCommitWithTheStep(t *testing.T) {
	ctx := context.Background()
	pool := testutil.PostgresPool(t)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	recorder := &branchOutputRecorder{}
	runtime := &WorkflowSerialRuntime{Transactions: pool, OutputRecorder: recorder}
	leg := FanoutLegTaskPayloadV1{GroupID: "group", LegID: "leg"}
	usage := fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{}}
	outputs := []deliverable.WorkflowOutput{{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "run", NodeID: "draft", Output: "初稿"}}
	if err := runtime.recordFanoutLegStep(ctx, "ws", leg, "draft", 0, json.RawMessage(`"初稿"`), nil, usage, outputs); err != nil {
		t.Fatal(err)
	}
	if len(recorder.recorded) != 1 || recorder.recorded[0].NodeID != "draft" {
		t.Fatalf("recorded outputs = %+v", recorder.recorded)
	}
	// A replay loses to the already committed step and must not append its
	// different output or depend on the output store being available.
	recorder.fail = errors.New("deliverable store unavailable")
	replayed := []deliverable.WorkflowOutput{{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "run", NodeID: "draft", Output: "另一份初稿"}}
	if err := runtime.recordFanoutLegStep(ctx, "ws", leg, "draft", 0, json.RawMessage(`"另一份初稿"`), nil, usage, replayed); err != nil {
		t.Fatalf("replaying a committed step wrote its outputs again: %v", err)
	}
	if len(recorder.recorded) != 1 || recorder.recorded[0].Output != "初稿" {
		t.Fatalf("replay changed the committed outputs: %+v", recorder.recorded)
	}
	recorder.fail = errors.New("deliverable store unavailable")
	failing := []deliverable.WorkflowOutput{{WorkspaceID: "ws", RunID: "run", RunSnapshotID: "run", NodeID: "check", Output: "核对"}}
	err := runtime.recordFanoutLegStep(ctx, "ws", leg, "check", 1, json.RawMessage(`"核对"`), nil, usage, failing)
	if executionErrorCode(err) != ErrorCodeDeliveryUnavailable {
		t.Fatalf("failed output recording = %v, want delivery unavailable", err)
	}
	finished, err := runtime.finishedFanoutLegSteps(ctx, "ws", leg)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := finished["check"]; kept || len(finished) != 1 {
		t.Fatalf("a step whose outputs failed to record was kept: %+v", finished)
	}
}
