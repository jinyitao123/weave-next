package execution

import (
	"context"
	"crypto/sha256"
	"fmt"
)

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

// InvocationID identifies one logical call across scheduler retries. Explicit
// workflow resumption supplies a new generation; a process restart does not.
type invocationContextKey struct{}

func WithInvocationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, invocationContextKey{}, id)
}

func InvocationID(ctx context.Context) string {
	id, _ := ctx.Value(invocationContextKey{}).(string)
	return id
}

// EngineTaskID is stable for one server-owned workflow invocation.
func EngineTaskID(workspaceID, invocationID string) string {
	if invocationID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(workspaceID + "\x00" + invocationID))
	return fmt.Sprintf("task-%x", sum[:16])
}

type inputTasksContextKey struct{}

func WithInputTaskIDs(ctx context.Context, ids []string) context.Context {
	return context.WithValue(ctx, inputTasksContextKey{}, append([]string(nil), ids...))
}
func InputTaskIDs(ctx context.Context) []string {
	ids, _ := ctx.Value(inputTasksContextKey{}).([]string)
	return append([]string(nil), ids...)
}

type attemptLineageKey struct{}
type attemptLineage struct{ Root, Parent string }

// WithAttemptLineage carries observation-only correlation. It must never be
// used for queue parent ownership, cancellation, or deadline inheritance.
func WithAttemptLineage(ctx context.Context, root, parent string) context.Context {
	return context.WithValue(ctx, attemptLineageKey{}, attemptLineage{root, parent})
}
func AttemptLineage(ctx context.Context) (string, string) {
	value, _ := ctx.Value(attemptLineageKey{}).(attemptLineage)
	return value.Root, value.Parent
}
