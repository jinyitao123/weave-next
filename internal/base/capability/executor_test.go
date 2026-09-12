package capability

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

type recordingStepExecutor struct{ ids []string }

func (e *recordingStepExecutor) ExecuteStep(_ context.Context, step PlanStep, _ json.RawMessage) (json.RawMessage, error) {
	e.ids = append(e.ids, step.ID)
	return json.RawMessage(`{"ok":true}`), nil
}

func TestExecutePlanRunsCompiledOrderAndAssemblesOutputs(t *testing.T) {
	plan := Plan{Steps: []PlanStep{{ID: "first"}, {ID: "second"}}}
	executor := &recordingStepExecutor{}
	result, err := ExecutePlan(context.Background(), plan, json.RawMessage(`{"input":1}`), executor)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(executor.ids, []string{"first", "second"}) {
		t.Fatalf("steps=%v", executor.ids)
	}
	var outputs map[string]json.RawMessage
	if err := json.Unmarshal(result, &outputs); err != nil || len(outputs) != 2 {
		t.Fatalf("outputs=%s err=%v", result, err)
	}
}

func TestExecutePlanHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecutePlan(ctx, Plan{Steps: []PlanStep{{ID: "first"}}}, json.RawMessage(`{}`), &recordingStepExecutor{})
	if err != context.Canceled {
		t.Fatalf("expected cancellation, got %v", err)
	}
}
