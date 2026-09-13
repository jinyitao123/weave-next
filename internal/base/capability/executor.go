package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

type StepExecutor interface {
	ExecuteStep(context.Context, PlanStep, json.RawMessage) (json.RawMessage, error)
}

// ExecutePlan evaluates dependency-ready steps together. A step sees only its
// declared inputs, or original run input when it declares no bindings.
func ExecutePlan(ctx context.Context, plan Plan, input json.RawMessage, executor StepExecutor) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if executor == nil || len(plan.Steps) == 0 {
		return nil, fmt.Errorf("executor and nonempty plan required")
	}
	if err := ValidateValue(plan.InputSchema, input); err != nil {
		return nil, fmt.Errorf("input schema: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	outputs := map[string]json.RawMessage{}
	for len(outputs) < len(plan.Steps) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ready := []PlanStep{}
		for _, step := range plan.Steps {
			if _, done := outputs[step.ID]; done {
				continue
			}
			available := true
			for _, dep := range step.Dependencies {
				if _, ok := outputs[dep]; !ok {
					available = false
				}
			}
			if available {
				ready = append(ready, step)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("plan has unresolved dependencies")
		}
		values := make([]json.RawMessage, len(ready))
		errs := make([]error, len(ready))
		var wg sync.WaitGroup
		for i, step := range ready {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if recover() != nil {
						errs[i] = fmt.Errorf("step executor panicked")
						cancel()
					}
				}()
				value, err := bindInputs(step.InputBindings, input, outputs)
				if err == nil {
					if step.Kind == StepTransform || step.Kind == StepDeliver {
						values[i] = value
					} else {
						values[i], err = executor.ExecuteStep(ctx, step, value)
					}
				}
				if err == nil && !json.Valid(values[i]) {
					err = fmt.Errorf("invalid JSON output")
				}
				if err == nil && len(step.OutputSchema) > 0 {
					err = ValidateValue(step.OutputSchema, values[i])
				}
				if err != nil {
					errs[i] = fmt.Errorf("step %s: %w", step.ID, err)
					cancel()
				}
			}()
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		for i, step := range ready {
			outputs[step.ID] = append(json.RawMessage(nil), values[i]...)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result, err := json.Marshal(outputs)
	if err != nil {
		return nil, err
	}
	if err := ValidateValue(plan.OutputSchema, result); err != nil {
		return nil, fmt.Errorf("output schema: %w", err)
	}
	return result, nil
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
		raw := input
		if ref.Source == "step_output" {
			raw = outputs[ref.StepID]
		}
		value, err := pointerValue(raw, ref.Path)
		if err != nil {
			return nil, err
		}
		result[key] = value
	}
	return json.Marshal(result)
}

func pointerValue(raw json.RawMessage, path string) (json.RawMessage, error) {
	if err := validPointer(path); err != nil {
		return nil, err
	}
	if path == "" {
		if !json.Valid(raw) {
			return nil, fmt.Errorf("missing input")
		}
		return raw, nil
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
				return nil, fmt.Errorf("input path not found")
			}
			raw = array[index]
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("input path not found")
		}
	}
	return raw, nil
}
