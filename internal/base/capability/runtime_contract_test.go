package capability

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPublishedDefinitionDetachesNestedDraft(t *testing.T) {
	d := fixtureDefinition()
	r, err := Publish(d, 1)
	if err != nil {
		t.Fatal(err)
	}
	d.Steps[0].Instruction = "changed"
	d.Roles[0].Name = "changed"
	d.InputSchema[0] = '!'
	if err := r.Validate(); err != nil {
		t.Fatalf("draft mutation changed publication: %v", err)
	}
}

func TestMalformedStepSchemaReturnsErrorWithoutPanic(t *testing.T) {
	d := fixtureDefinition()
	d.Steps[0].OutputSchema = json.RawMessage(`{`)
	if _, err := Publish(d, 1); err == nil {
		t.Fatal("malformed schema accepted")
	}
}

type parallelExecutor struct {
	entered chan string
	release chan struct{}
}

func (e parallelExecutor) ExecuteStep(ctx context.Context, step PlanStep, input json.RawMessage) (json.RawMessage, error) {
	e.entered <- step.ID
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return json.RawMessage(`{"value":7}`), nil
}
func TestParallelJoinPreservesBindingsAndOriginalInput(t *testing.T) {
	d := fixtureDefinition()
	d.Steps[1].MaxIterations = 0
	d.Steps = append(d.Steps, Step{ID: "join", Name: "join", RoleID: "author", Kind: StepTransform, InputBindings: map[string]ValueRef{"a": {Source: "step_output", StepID: "write", Path: "/value"}, "b": {Source: "step_output", StepID: "check", Path: "/value"}, "source": {Source: "input", Path: "/source"}}})
	d.Relations = []Relation{{From: "write", To: "join", Kind: RelationJoin}, {From: "check", To: "join", Kind: RelationJoin}}
	r, err := Publish(d, 1)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		result, err := ExecutePlan(t.Context(), plan, json.RawMessage(`{"source":42}`), parallelExecutor{entered, release})
		if err == nil {
			var obj struct{ Join struct{ A, B, Source int } }
			err = json.Unmarshal(result, &obj)
			if obj.Join.A != 7 || obj.Join.B != 7 || obj.Join.Source != 42 {
				t.Errorf("bindings=%s", result)
			}
		}
		done <- err
	}()
	<-entered
	<-entered
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCompileRejectsUnsupportedSemanticsAndBadBindings(t *testing.T) {
	for _, kind := range []string{"loop", "condition", "tool", "future", "schema"} {
		t.Run(kind, func(t *testing.T) {
			d := fixtureDefinition()
			d.Steps[1].MaxIterations = 0
			switch kind {
			case "loop":
				d.Relations[0].Kind = RelationLoop
				d.Steps[1].MaxIterations = 2
			case "condition":
				d.Relations[0].Kind = RelationCondition
			case "tool":
				d.Resources.ToolIDs = []string{"unbound"}
			case "future":
				d.Steps[0].InputBindings = map[string]ValueRef{"bad": {Source: "step_output", StepID: "check"}}
			case "schema":
				d.OutputSchema = json.RawMessage(`{"type":"invalid-type"}`)
			}
			r, err := Publish(d, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Compile(r); err == nil {
				t.Fatal("unsupported definition compiled")
			}
		})
	}
}
