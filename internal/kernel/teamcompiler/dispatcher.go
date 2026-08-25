package teamcompiler

import (
	"context"

	"github.com/jinyitao123/loom/contract"
)

var workerForbiddenTools = map[string]struct{}{
	"delegate":          {},
	"dispatch_parallel": {},
	"transfer_to":       {},
}

type lockedWorkerDispatcher struct {
	downstream contract.ToolDispatcher
}

func NewLockedWorkerDispatcher(downstream contract.ToolDispatcher) contract.ToolDispatcher {
	return &lockedWorkerDispatcher{downstream: downstream}
}

func (d *lockedWorkerDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	if d == nil || d.downstream == nil {
		return []contract.ToolDef{}, nil
	}
	tools, err := d.downstream.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	filtered := make([]contract.ToolDef, 0, len(tools))
	for _, tool := range tools {
		if _, forbidden := workerForbiddenTools[tool.Name]; forbidden {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered, nil
}

func (d *lockedWorkerDispatcher) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if _, forbidden := workerForbiddenTools[call.Name]; forbidden {
		return nil, ErrTeamInteractionUnauthorized
	}
	if d == nil || d.downstream == nil {
		return nil, ErrTeamInteractionUnauthorized
	}
	return d.downstream.Dispatch(ctx, call)
}

var _ contract.ToolDispatcher = (*lockedWorkerDispatcher)(nil)
