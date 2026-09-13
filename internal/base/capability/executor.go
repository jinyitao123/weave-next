package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

type StepExecutor interface {
	ExecuteStep(context.Context, PlanStep, json.RawMessage) (json.RawMessage, error)
}

type ToolStepExecutor interface {
	ExecuteTool(context.Context, PlanStep, json.RawMessage) (json.RawMessage, error)
}

// ExecutionState is the durable, engine-independent checkpoint. Persisting it
// after each transition makes model and tool steps replay-safe after restart.
type ExecutionState struct {
	Outputs    map[string]json.RawMessage `json:"outputs,omitempty"`
	Skipped    map[string]bool            `json:"skipped,omitempty"`
	Iterations map[string]int             `json:"iterations,omitempty"`
}

type ExecutionEvent struct {
	Type       string          `json:"type"`
	StepID     string          `json:"step_id,omitempty"`
	StepKind   StepKind        `json:"step_kind,omitempty"`
	Output     json.RawMessage `json:"output,omitempty"`
	Iterations int             `json:"iterations,omitempty"`
}

type ExecutionObserver interface {
	Checkpoint(context.Context, ExecutionState, ExecutionEvent) error
}

type PauseError struct {
	StepID string
	Title  string
	Schema json.RawMessage
}

func (e *PauseError) Error() string { return "human confirmation required" }
func IsPause(err error) bool        { var pause *PauseError; return errors.As(err, &pause) }

// ResumeHuman records the reviewed value into a checkpoint. Execution resumes
// at the next unfinished step and never replays completed model or tool work.
func ResumeHuman(state ExecutionState, stepID string, value json.RawMessage) (ExecutionState, error) {
	if strings.TrimSpace(stepID) == "" || !json.Valid(value) {
		return state, errors.New("valid step and JSON response required")
	}
	state = normalizeState(state)
	state.Outputs[stepID] = append(json.RawMessage(nil), value...)
	delete(state.Skipped, stepID)
	return state, nil
}

func ExecutePlan(ctx context.Context, plan Plan, input json.RawMessage, executor StepExecutor) (json.RawMessage, error) {
	result, _, err := ExecutePlanResumable(ctx, plan, input, executor, ExecutionState{}, nil)
	return result, err
}

func ExecutePlanResumable(ctx context.Context, plan Plan, input json.RawMessage, executor StepExecutor, state ExecutionState, observer ExecutionObserver) (json.RawMessage, ExecutionState, error) {
	if err := ctx.Err(); err != nil {
		return nil, state, err
	}
	if executor == nil || len(plan.Steps) == 0 {
		return nil, state, errors.New("executor and nonempty plan required")
	}
	if err := ValidateValue(plan.InputSchema, input); err != nil {
		return nil, state, fmt.Errorf("input schema: %w", err)
	}
	state = normalizeState(state)
	positions := map[string]int{}
	for i, step := range plan.Steps {
		positions[step.ID] = i
	}

	for completedCount(state) < len(plan.Steps) {
		if err := ctx.Err(); err != nil {
			return nil, state, err
		}
		progressed := false
		for _, step := range plan.Steps {
			if stepDone(state, step.ID) || !dependenciesDone(state, step.Dependencies) {
				continue
			}
			if branchDisabled(plan.Relations, state, step.ID) {
				state.Skipped[step.ID] = true
				if err := observe(ctx, observer, state, ExecutionEvent{Type: "step_skipped", StepID: step.ID, StepKind: step.Kind}); err != nil {
					return nil, state, err
				}
				progressed = true
			}
		}
		ready := []PlanStep{}
		for _, step := range plan.Steps {
			if !stepDone(state, step.ID) && dependenciesDone(state, step.Dependencies) {
				ready = append(ready, step)
			}
		}
		if len(ready) == 0 {
			if progressed {
				continue
			}
			return nil, state, errors.New("plan has unresolved dependencies")
		}
		// A human wait is a durable boundary. Finish other independent ready work
		// first, then park with all completed work checkpointed.
		executable := ready[:0]
		var waiting *PlanStep
		for i := range ready {
			if ready[i].Kind == StepWait {
				if waiting == nil {
					copy := ready[i]
					waiting = &copy
				}
				continue
			}
			executable = append(executable, ready[i])
		}
		if len(executable) == 0 && waiting != nil {
			pause := &PauseError{StepID: waiting.ID, Title: waiting.ApprovalTitle, Schema: append(json.RawMessage(nil), waiting.OutputSchema...)}
			if err := observe(ctx, observer, state, ExecutionEvent{Type: "human_waiting", StepID: waiting.ID, StepKind: waiting.Kind}); err != nil {
				return nil, state, err
			}
			return nil, state, pause
		}
		type outcome struct {
			value json.RawMessage
			err   error
		}
		results := make([]outcome, len(executable))
		snapshot := cloneState(state)
		var wg sync.WaitGroup
		for i, step := range executable {
			i, step := i, step
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if recover() != nil {
						results[i].err = errors.New("step executor panicked")
					}
				}()
				bound, err := bindInputs(step.InputBindings, input, snapshot.Outputs)
				if err != nil {
					results[i].err = err
					return
				}
				var value json.RawMessage
				switch step.Kind {
				case StepTransform, StepDeliver:
					value = bound
				case StepCondition:
					var truth bool
					truth, err = evaluatePredicate(*step.Condition, input, snapshot.Outputs)
					if err == nil {
						value, _ = json.Marshal(truth)
					}
				case StepTool:
					tools, ok := executor.(ToolStepExecutor)
					if !ok {
						err = errors.New("tool executor unavailable")
					} else {
						value, err = tools.ExecuteTool(ctx, step, bound)
					}
				case StepWorker:
					value, err = executor.ExecuteStep(ctx, step, bound)
				default:
					err = fmt.Errorf("unsupported step kind %s", step.Kind)
				}
				if err == nil && !json.Valid(value) {
					err = errors.New("invalid JSON output")
				}
				if err == nil && len(step.OutputSchema) > 0 {
					err = ValidateValue(step.OutputSchema, value)
				}
				results[i] = outcome{value: value, err: err}
			}()
		}
		wg.Wait()
		for i, step := range executable {
			if results[i].err != nil {
				return nil, state, fmt.Errorf("step %s: %w", step.ID, results[i].err)
			}
		}
		for i, step := range executable {
			state.Outputs[step.ID] = append(json.RawMessage(nil), results[i].value...)
			if err := observe(ctx, observer, state, ExecutionEvent{Type: "step_completed", StepID: step.ID, StepKind: step.Kind, Output: results[i].value}); err != nil {
				return nil, state, err
			}
			progressed = true
		}
		looped := false
		for _, step := range executable {
			yes, err := applyLoops(ctx, plan, step, positions, &state, observer)
			if err != nil {
				return nil, state, err
			}
			if yes {
				looped = true
				break
			}
		}
		if looped {
			continue
		}
		if !progressed {
			return nil, state, errors.New("plan has unresolved dependencies")
		}
	}
	var result json.RawMessage
	var err error
	if plan.Result != nil {
		result, err = resolveRef(*plan.Result, input, state.Outputs)
	} else {
		result, err = json.Marshal(state.Outputs)
	}
	if err != nil {
		return nil, state, fmt.Errorf("result: %w", err)
	}
	if err := ValidateValue(plan.OutputSchema, result); err != nil {
		return nil, state, fmt.Errorf("output schema: %w", err)
	}
	if err := observe(ctx, observer, state, ExecutionEvent{Type: "execution_completed", Output: result}); err != nil {
		return nil, state, err
	}
	return result, state, nil
}
func normalizeState(state ExecutionState) ExecutionState {
	if state.Outputs == nil {
		state.Outputs = map[string]json.RawMessage{}
	}
	if state.Skipped == nil {
		state.Skipped = map[string]bool{}
	}
	if state.Iterations == nil {
		state.Iterations = map[string]int{}
	}
	return state
}
func stepDone(state ExecutionState, id string) bool {
	_, ok := state.Outputs[id]
	return ok || state.Skipped[id]
}
func completedCount(state ExecutionState) int {
	count := len(state.Outputs)
	for id, yes := range state.Skipped {
		if yes {
			if _, ok := state.Outputs[id]; !ok {
				count++
			}
		}
	}
	return count
}
func dependenciesDone(state ExecutionState, deps []string) bool {
	for _, id := range deps {
		if !stepDone(state, id) {
			return false
		}
	}
	return true
}
func branchDisabled(relations []Relation, state ExecutionState, target string) bool {
	for _, rel := range relations {
		if rel.To != target || rel.Kind != RelationCondition || rel.When == nil {
			continue
		}
		raw, ok := state.Outputs[rel.From]
		if !ok {
			continue
		}
		var actual bool
		if json.Unmarshal(raw, &actual) != nil || actual != *rel.When {
			return true
		}
	}
	return false
}
func applyLoops(ctx context.Context, plan Plan, source PlanStep, positions map[string]int, state *ExecutionState, observer ExecutionObserver) (bool, error) {
	for _, rel := range plan.Relations {
		if rel.Kind != RelationLoop || rel.From != source.ID {
			continue
		}
		var repeat bool
		if raw := state.Outputs[source.ID]; json.Unmarshal(raw, &repeat) != nil {
			return false, fmt.Errorf("loop source %s must output a boolean", source.ID)
		}
		if !repeat {
			continue
		}
		limit := plan.Steps[positions[rel.To]].MaxIterations
		key := rel.From + "->" + rel.To
		if state.Iterations[key] >= limit {
			return false, fmt.Errorf("loop %s exceeded max_iterations %d", key, limit)
		}
		state.Iterations[key]++
		from, to := positions[rel.To], positions[rel.From]
		for i := from; i <= to; i++ {
			delete(state.Outputs, plan.Steps[i].ID)
			delete(state.Skipped, plan.Steps[i].ID)
		}
		if err := observe(ctx, observer, *state, ExecutionEvent{Type: "loop_restarted", StepID: rel.To, Iterations: state.Iterations[key]}); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
func observe(ctx context.Context, observer ExecutionObserver, state ExecutionState, event ExecutionEvent) error {
	if observer == nil {
		return nil
	}
	return observer.Checkpoint(ctx, cloneState(state), event)
}
func cloneState(state ExecutionState) ExecutionState {
	out := normalizeState(ExecutionState{})
	for k, v := range state.Outputs {
		out.Outputs[k] = append(json.RawMessage(nil), v...)
	}
	for k, v := range state.Skipped {
		out.Skipped[k] = v
	}
	for k, v := range state.Iterations {
		out.Iterations[k] = v
	}
	return out
}

func evaluatePredicate(p Predicate, input json.RawMessage, outputs map[string]json.RawMessage) (bool, error) {
	left, err := resolveRef(p.Left, input, outputs)
	if err != nil {
		return false, err
	}
	switch p.Operator {
	case "truthy":
		return truthy(left), nil
	case "empty":
		return !truthy(left), nil
	}
	if p.Right == nil {
		return false, errors.New("predicate right value required")
	}
	right, err := resolveRef(*p.Right, input, outputs)
	if err != nil {
		return false, err
	}
	switch p.Operator {
	case "eq":
		return bytes.Equal(canonical(left), canonical(right)), nil
	case "ne":
		return !bytes.Equal(canonical(left), canonical(right)), nil
	case "gt", "gte", "lt", "lte":
		var a, b float64
		if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
			return false, errors.New("numeric predicate requires numbers")
		}
		switch p.Operator {
		case "gt":
			return a > b, nil
		case "gte":
			return a >= b, nil
		case "lt":
			return a < b, nil
		default:
			return a <= b, nil
		}
	default:
		return false, errors.New("unsupported predicate operator")
	}
}
func canonical(raw json.RawMessage) []byte {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	b, _ := json.Marshal(v)
	return b
}
func truthy(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}

func validPointer(path string) error {
	if path != "" && !strings.HasPrefix(path, "/") {
		return fmt.Errorf("%w: path must be a JSON pointer", ErrInvalidRevision)
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '~' {
			i++
			if i == len(path) || (path[i] != '0' && path[i] != '1') {
				return fmt.Errorf("%w: invalid pointer escape", ErrInvalidRevision)
			}
		}
	}
	return nil
}
func bindInputs(bindings map[string]ValueRef, input json.RawMessage, outputs map[string]json.RawMessage) (json.RawMessage, error) {
	if len(bindings) == 0 {
		return append(json.RawMessage(nil), input...), nil
	}
	result := map[string]json.RawMessage{}
	for key, ref := range bindings {
		v, err := resolveRef(ref, input, outputs)
		if err != nil {
			return nil, err
		}
		result[key] = v
	}
	return json.Marshal(result)
}
func resolveRef(ref ValueRef, input json.RawMessage, outputs map[string]json.RawMessage) (json.RawMessage, error) {
	var raw json.RawMessage
	switch ref.Source {
	case "input":
		raw = input
	case "step_output":
		raw = outputs[ref.StepID]
	case "literal":
		raw = ref.Literal
	default:
		return nil, errors.New("unsupported value source")
	}
	return pointerValue(raw, ref.Path)
}
func pointerValue(raw json.RawMessage, path string) (json.RawMessage, error) {
	if err := validPointer(path); err != nil {
		return nil, err
	}
	if path == "" {
		if !json.Valid(raw) {
			return nil, errors.New("missing input")
		}
		return append(json.RawMessage(nil), raw...), nil
	}
	for _, token := range strings.Split(path[1:], "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) == nil && object != nil {
			raw = object[token]
		} else {
			var array []json.RawMessage
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || strconv.Itoa(index) != token || json.Unmarshal(raw, &array) != nil || index >= len(array) {
				return nil, errors.New("input path not found")
			}
			raw = array[index]
		}
		if len(raw) == 0 {
			return nil, errors.New("input path not found")
		}
	}
	return raw, nil
}
