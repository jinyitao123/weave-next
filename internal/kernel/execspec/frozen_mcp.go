package execspec

import (
	"context"
	"encoding/json"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

// FrozenMCPInvocation is server-owned execution authority. It is carried from
// the published workflow loader to admission, never read from model arguments.
type FrozenMCPInvocation struct {
	WorkspaceID   string                    `json:"workspace_id"`
	AgentID       string                    `json:"agent_id"`
	AgentVersion  int64                     `json:"agent_version"`
	RunSnapshotID string                    `json:"run_snapshot_id"`
	FactoryKey    frozen.FactoryKey         `json:"factory_key"`
	Bindings      []frozen.FrozenMCPBinding `json:"bindings"`
}

type frozenMCPContextKey struct{}

func WithFrozenMCPInvocation(ctx context.Context, invocation FrozenMCPInvocation) context.Context {
	// Store bytes to isolate the authority from mutable caller slices and maps.
	raw, err := json.Marshal(invocation)
	if err != nil {
		panic("invalid frozen MCP invocation")
	}
	return context.WithValue(ctx, frozenMCPContextKey{}, raw)
}

func FrozenMCPInvocationFromContext(ctx context.Context) *FrozenMCPInvocation {
	raw, ok := ctx.Value(frozenMCPContextKey{}).([]byte)
	if !ok {
		return nil
	}
	var invocation FrozenMCPInvocation
	if err := json.Unmarshal(raw, &invocation); err != nil {
		panic("invalid frozen MCP context")
	}
	return &invocation
}
