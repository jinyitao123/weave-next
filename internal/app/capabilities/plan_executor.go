package capabilities

import (
	"context"
	"encoding/json"

	"github.com/jinyitao123/weave/internal/base/capability"
)

// PlanTaskExecutor adapts the durable invocation worker to one runtime's
// single-step executor. The runtime decides how a worker role is hosted.
type PlanTaskExecutor struct {
	Steps capability.StepExecutor
}

func (e PlanTaskExecutor) Execute(ctx context.Context, task InvocationTask) (json.RawMessage, error) {
	return capability.ExecutePlan(ctx, task.Plan, task.Input, e.Steps)
}
