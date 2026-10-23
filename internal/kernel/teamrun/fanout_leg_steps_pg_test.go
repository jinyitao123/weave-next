package teamrun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jinyitao123/weave/internal/base/db"
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
	if err := runtime.recordFanoutLegStep(ctx, "ws", leg, "draft", 0, json.RawMessage(`"初稿"`), []string{"member:draft-run"}, fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{}}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.recordFanoutLegStep(ctx, "ws", leg, "draft", 0, json.RawMessage(`"另一份初稿"`), nil, fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{}}); err != nil {
		t.Fatal(err)
	}
	other := FanoutLegTaskPayloadV1{GroupID: "group", LegID: "other-leg"}
	if err := runtime.recordFanoutLegStep(ctx, "ws", other, "check", 0, json.RawMessage(`"核对"`), []string{}, fanoutLegStepUsageV1{SchemaVersion: 1, Attempts: []nodeUsageAttempt{}}); err != nil {
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
