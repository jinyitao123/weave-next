package businessaction

import (
	"context"
	"errors"
	"fmt"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

type taskScopedActionHost struct {
	store           *Store
	inputRevisionID string
	requested       []string
	contract        *mcphost.ToolContract
}

func (h *taskScopedActionHost) ListTools(context.Context) ([]contract.ToolDef, error) {
	return nil, nil
}
func (h *taskScopedActionHost) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	return h.DispatchWithStart(ctx, call, nil)
}
func (h *taskScopedActionHost) DispatchWithStart(ctx context.Context, call contract.ToolCall, start func(context.Context) error) (*contract.ToolResult, error) {
	bound, err := h.store.resolve(ctx, h.requested)
	if err != nil {
		return nil, err
	}
	defer clear(bound.token)
	if bound.inputRevisionID != h.inputRevisionID {
		return nil, errors.New("task authorization changed frozen input")
	}
	host := mcphost.NewHTTPHost(bound.issuer+TaskDelegationPath+"/mcp", mcphost.WithHeaders(map[string]string{"Authorization": "Bearer " + string(bound.token)}), mcphost.WithFilter([]string{"list_actions", "run_action"}), mcphost.WithToolContract(h.contract), mcphost.WithUnknownDispatchOutcome(), mcphost.WithDispatchGuard(func(guardCtx context.Context) error {
		fresh, err := h.store.resolve(guardCtx, h.requested)
		if err != nil {
			return err
		}
		defer clear(fresh.token)
		if fresh.inputRevisionID != bound.inputRevisionID || fresh.generation != bound.generation {
			return execution.NewAuthorizationRefusal(bound.inputRevisionID, bound.generation, ErrDelegationExpired)
		}
		if start != nil {
			return start(guardCtx)
		}
		return nil
	}))
	return host.Dispatch(ctx, call)
}

// CanRetryAuthorization checks both durable claim and online task authority;
// a changed expiry in the database cannot authorize sending an old token.
func (s *Store) CanRetryAuthorization(ctx context.Context, proof execution.AuthorizationRefusal) (bool, error) {
	if !proof.Renewable() {
		return false, nil
	}
	current, err := s.resolve(ctx, nil)
	if err != nil {
		return false, err
	}
	defer clear(current.token)
	if current.inputRevisionID != proof.InputRevisionID {
		return false, fmt.Errorf("%w: renewed task input differs", mcphost.ErrFailClosed)
	}
	return current.generation > proof.Generation, nil
}
