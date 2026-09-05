package execution

import "context"

type nodeContextKey struct{}

// WithNodeID carries the server-selected workflow node into its durable task.
// A runtime may report facts about this node; it cannot choose another node.
func WithNodeID(ctx context.Context, nodeID string) context.Context {
	return context.WithValue(ctx, nodeContextKey{}, nodeID)
}

func NodeID(ctx context.Context) string {
	id, _ := ctx.Value(nodeContextKey{}).(string)
	return id
}
