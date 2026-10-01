package execution

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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

// WithOperationID binds one server-owned durable tool journal slot. A model
// tool-call identifier is never used as this identity.
type operationContextKey struct{}

func WithOperationID(ctx context.Context, slot string) context.Context {
	return context.WithValue(ctx, operationContextKey{}, slot)
}

func OperationID(ctx context.Context) string {
	id, _ := ctx.Value(operationContextKey{}).(string)
	return id
}

// EngineOperationID scopes a journal slot to its frozen input and capability.
// Retrying that slot keeps the key; a new slot is a distinct operation even
// when all business parameters are identical.
func EngineOperationID(inputRevisionID, invocationID, slot, capabilityID string) string {
	if inputRevisionID == "" || invocationID == "" || slot == "" || capabilityID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(inputRevisionID + "\x00" + invocationID + "\x00" + slot + "\x00" + capabilityID))
	return fmt.Sprintf("weave-op-%x", sum[:])
}

// OperationReconciler reads a previously committed host receipt. It must never
// perform an external operation or infer success from model text.
type OperationReconciler func(context.Context, string, json.RawMessage) (json.RawMessage, bool, error)
type operationReconcilerContextKey struct{}

func WithOperationReconciler(ctx context.Context, reconcile OperationReconciler) context.Context {
	return context.WithValue(ctx, operationReconcilerContextKey{}, reconcile)
}

func ReconcileOperation(ctx context.Context, slot string, input json.RawMessage) (json.RawMessage, bool, error) {
	reconcile, _ := ctx.Value(operationReconcilerContextKey{}).(OperationReconciler)
	if reconcile == nil {
		return nil, false, nil
	}
	return reconcile(ctx, slot, input)
}
