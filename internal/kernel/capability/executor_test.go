package capability

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
)

type recordingStepExecutor struct{ ids []string }

func (e *recordingStepExecutor) ExecuteStep(_ context.Context, step PlanStep, input json.RawMessage) (json.RawMessage, error) {
	e.ids = append(e.ids, step.ID)
	if step.ID == "final" {
		return json.RawMessage(`{"answer":"done"}`), nil
	}
	return json.RawMessage(`{"ok":true}`), nil
}
func (e *recordingStepExecutor) ExecuteTool(_ context.Context, step PlanStep, input json.RawMessage) (json.RawMessage, error) {
	e.ids = append(e.ids, "tool:"+step.ToolID)
	return input, nil
}

type recordingObserver struct {
	states []ExecutionState
	events []ExecutionEvent
}

func (o *recordingObserver) Checkpoint(_ context.Context, state ExecutionState, event ExecutionEvent) error {
	o.states = append(o.states, state)
	o.events = append(o.events, event)
	return nil
}

func TestExecutePlanReturnsDeclaredBusinessResult(t *testing.T) {
	plan := Plan{InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object","required":["answer"]}`), Result: &ValueRef{Source: "step_output", StepID: "final"}, Steps: []PlanStep{{ID: "first", Kind: StepWorker}, {ID: "final", Kind: StepWorker, Dependencies: []string{"first"}}}}
	exec := &recordingStepExecutor{}
	result, err := ExecutePlan(context.Background(), plan, json.RawMessage(`{"input":1}`), exec)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"answer":"done"}` {
		t.Fatalf("result=%s", result)
	}
	if !reflect.DeepEqual(exec.ids, []string{"first", "final"}) {
		t.Fatalf("steps=%v", exec.ids)
	}
}
func TestExecutePlanConditionAndTool(t *testing.T) {
	yes, no := true, false
	plan := Plan{InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Result: &ValueRef{Source: "step_output", StepID: "tool"}, Steps: []PlanStep{
		{ID: "condition", Kind: StepCondition, Condition: &Predicate{Left: ValueRef{Source: "input", Path: "/approved"}, Operator: "truthy"}},
		{ID: "tool", Kind: StepTool, ToolID: "web.fetch", Dependencies: []string{"condition"}, InputBindings: map[string]ValueRef{"url": {Source: "input", Path: "/url"}}},
		{ID: "reject", Kind: StepTransform, Dependencies: []string{"condition"}, InputBindings: map[string]ValueRef{"rejected": {Source: "literal", Literal: json.RawMessage(`true`)}}},
	}, Relations: []Relation{{From: "condition", To: "tool", Kind: RelationCondition, When: &yes}, {From: "condition", To: "reject", Kind: RelationCondition, When: &no}}}
	exec := &recordingStepExecutor{}
	result, err := ExecutePlan(context.Background(), plan, json.RawMessage(`{"approved":true,"url":"https://example.com"}`), exec)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"url":"https://example.com"}` {
		t.Fatalf("result=%s", result)
	}
	if !reflect.DeepEqual(exec.ids, []string{"tool:web.fetch"}) {
		t.Fatalf("ids=%v", exec.ids)
	}
}
func TestExecutePlanHumanPauseAndResumeKeepsCheckpoint(t *testing.T) {
	plan := Plan{InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Result: &ValueRef{Source: "step_output", StepID: "after"}, Steps: []PlanStep{{ID: "before", Kind: StepWorker}, {ID: "approval", Kind: StepWait, ApprovalTitle: "请确认", Dependencies: []string{"before"}, OutputSchema: json.RawMessage(`{"type":"object"}`)}, {ID: "after", Kind: StepTransform, Dependencies: []string{"approval"}, InputBindings: map[string]ValueRef{"decision": {Source: "step_output", StepID: "approval"}}}}}
	exec := &recordingStepExecutor{}
	obs := &recordingObserver{}
	_, state, err := ExecutePlanResumable(context.Background(), plan, json.RawMessage(`{}`), exec, ExecutionState{}, obs)
	var pause *PauseError
	if !errors.As(err, &pause) || pause.StepID != "approval" {
		t.Fatalf("err=%v", err)
	}
	state, err = ResumeHuman(state, "approval", json.RawMessage(`{"approved":true}`))
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := ExecutePlanResumable(context.Background(), plan, json.RawMessage(`{}`), exec, state, obs)
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"decision":{"approved":true}}` {
		t.Fatalf("result=%s", result)
	}
	if !reflect.DeepEqual(exec.ids, []string{"before"}) {
		t.Fatalf("completed worker replayed: %v", exec.ids)
	}
}
func TestExecutePlanHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExecutePlan(ctx, Plan{Steps: []PlanStep{{ID: "first"}}}, json.RawMessage(`{}`), &recordingStepExecutor{})
	if err != context.Canceled {
		t.Fatalf("got %v", err)
	}
}

type parallelRecoveryExecutor struct {
	mu       sync.Mutex
	calls    map[string]int
	failOnce bool
	ids      map[string][]string
}

func (e *parallelRecoveryExecutor) ExecuteStep(ctx context.Context, step PlanStep, _ json.RawMessage) (json.RawMessage, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls[step.ID]++
	e.ids[step.ID] = append(e.ids[step.ID], execution.InvocationID(ctx))
	if step.ID == "failed" && e.failOnce {
		e.failOnce = false
		return nil, errors.New("temporary failure")
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func TestParallelFailureCheckpointsSuccessAndReusesActivation(t *testing.T) {
	plan := Plan{InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Steps: []PlanStep{
		{ID: "confirmed", Kind: StepWorker}, {ID: "failed", Kind: StepWorker},
	}}
	exec := &parallelRecoveryExecutor{calls: map[string]int{}, ids: map[string][]string{}, failOnce: true}
	ctx := execution.WithInvocationID(t.Context(), "invocation")
	_, state, err := ExecutePlanResumable(ctx, plan, json.RawMessage(`{}`), exec, ExecutionState{}, &recordingObserver{})
	if err == nil || string(state.Outputs["confirmed"]) != `{"ok":true}` || state.Outputs["failed"] != nil {
		t.Fatalf("first state=%+v err=%v", state, err)
	}
	_, _, err = ExecutePlanResumable(ctx, plan, json.RawMessage(`{}`), exec, state, &recordingObserver{})
	if err != nil {
		t.Fatal(err)
	}
	if exec.calls["confirmed"] != 1 || exec.calls["failed"] != 2 {
		t.Fatalf("calls=%v", exec.calls)
	}
	if len(exec.ids["failed"]) != 2 || exec.ids["failed"][0] == "" || exec.ids["failed"][0] != exec.ids["failed"][1] {
		t.Fatalf("failed activation changed across recovery: %v", exec.ids["failed"])
	}
	if exec.ids["confirmed"][0] == exec.ids["failed"][0] {
		t.Fatal("parallel steps shared one activation identity")
	}
}
