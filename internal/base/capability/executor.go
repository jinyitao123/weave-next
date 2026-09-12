package capability

import (
	"context"
	"encoding/json"
	"fmt"
)

// StepExecutor is the only engine-specific seam required by the capability
// kernel. Loom, Codex, or another runtime executes one already-frozen step.
type StepExecutor interface {
	ExecuteStep(context.Context, PlanStep, json.RawMessage) (json.RawMessage, error)
}

// ExecutePlan runs the frozen plan in compiled order and returns a stable
// object keyed by step ID. Scheduling policies remain in the plan compiler and
// the runtime adapter; this function owns cancellation and result assembly.
func ExecutePlan(ctx context.Context, plan Plan, input json.RawMessage, executor StepExecutor) (json.RawMessage, error) {
	if executor == nil {
		return nil, fmt.Errorf("capability step executor is required")
	}
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	var current map[string]json.RawMessage
	if err := json.Unmarshal(input, &current); err != nil || current == nil {
		return nil, fmt.Errorf("capability input must be a JSON object")
	}
	outputs := make(map[string]json.RawMessage, len(plan.Steps))
	for _, step := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stepInput, err := json.Marshal(map[string]any{"input": current, "outputs": outputs})
		if err != nil {
			return nil, fmt.Errorf("marshal step %s input: %w", step.ID, err)
		}
		output, err := executor.ExecuteStep(ctx, step, stepInput)
		if err != nil {
			return nil, fmt.Errorf("execute capability step %s: %w", step.ID, err)
		}
		if len(output) == 0 || !json.Valid(output) {
			return nil, fmt.Errorf("execute capability step %s: output must be valid JSON", step.ID)
		}
		outputs[step.ID] = append(json.RawMessage(nil), output...)
		current = map[string]json.RawMessage{"value": output}
	}
	result, err := json.Marshal(outputs)
	if err != nil {
		return nil, fmt.Errorf("marshal capability outputs: %w", err)
	}
	return result, nil
}
