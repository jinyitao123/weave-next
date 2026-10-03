package compiler

import "context"

type NodeCompletionCheck func(context.Context, string) (bool, string, error)
type nodeCompletionBinding struct {
	policyID   string
	check      NodeCompletionCheck
	beforeTool func(context.Context) error
}
type nodeCompletionKey struct{}

// The workflow owner supplies a read-only verifier for one final source node.
// The policy identity is frozen into the existing Loom continuation fingerprint.
func WithNodeCompletionCheck(ctx context.Context, policyID string, check NodeCompletionCheck, beforeTool ...func(context.Context) error) context.Context {
	binding := nodeCompletionBinding{policyID: policyID, check: check}
	if len(beforeTool) > 0 {
		binding.beforeTool = beforeTool[0]
	}
	return context.WithValue(ctx, nodeCompletionKey{}, binding)
}
func nodeCompletionCheck(ctx context.Context) (string, NodeCompletionCheck, func(context.Context) error) {
	binding, _ := ctx.Value(nodeCompletionKey{}).(nodeCompletionBinding)
	return binding.policyID, binding.check, binding.beforeTool
}

// NodeCompletionCheckError is never a transport/retry signal. Its text contains
// only a bounded reason code, not a model answer or an underlying database error.
type NodeCompletionCheckError struct{ Reason string }

func (e *NodeCompletionCheckError) Error() string {
	return "declared completion check failed: " + e.Reason
}
