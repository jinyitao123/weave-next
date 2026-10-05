package compiler

import (
	"context"

	"github.com/jinyitao123/loom/contract"
)

type NodeCompletionCheck func(context.Context, string) (bool, string, error)

// NodeCompletionVerdict is a read-only completion decision. Reason is a bounded
// machine code forwarded to Loom, never shown to the model.
type NodeCompletionVerdict struct {
	Accepted bool
	Feedback string
	Reason   string
}

// NodeCompletionCandidate is the proposed final content with the transcript
// that led to it. PriorRejections holds the Reason of each earlier rejected
// candidate in the same loop, kept by Loom outside the transcript.
type NodeCompletionCandidate struct {
	Content         string
	Transcript      []contract.Message
	PriorRejections []string
}

// NodeCompletionReview sees the whole candidate, so once-per-loop rules use
// the structured rejection history that survives pauses, compaction and replay.
type NodeCompletionReview func(context.Context, NodeCompletionCandidate) (NodeCompletionVerdict, error)

// NodeToolChoiceInput describes one model round: the offered tools and whether
// the previous round's completion was rejected, with the verdict's Reason.
type NodeToolChoiceInput struct {
	Tools                     []contract.ToolDef
	CompletionRejected        bool
	CompletionRejectionReason string
}

// NodeToolChoice selects an optional tool constraint for one model round of
// the final source node. It must decide from authoritative host facts only.
type NodeToolChoice func(context.Context, NodeToolChoiceInput) (*contract.ToolChoice, error)

// NodeCompletionPolicy bundles one declared check's read-only hooks.
type NodeCompletionPolicy struct {
	Review     NodeCompletionReview
	BeforeTool func(context.Context) error
	ChooseTool NodeToolChoice
}

type nodeCompletionBinding struct {
	policyID string
	policy   NodeCompletionPolicy
}
type nodeCompletionKey struct{}

// The workflow owner supplies a read-only verifier for one final source node.
// The policy identity is frozen into the existing Loom continuation fingerprint.
func WithNodeCompletionCheck(ctx context.Context, policyID string, check NodeCompletionCheck, beforeTool ...func(context.Context) error) context.Context {
	policy := NodeCompletionPolicy{Review: func(ctx context.Context, candidate NodeCompletionCandidate) (NodeCompletionVerdict, error) {
		accepted, feedback, err := check(ctx, candidate.Content)
		return NodeCompletionVerdict{Accepted: accepted, Feedback: feedback}, err
	}}
	if len(beforeTool) > 0 {
		policy.BeforeTool = beforeTool[0]
	}
	return WithNodeCompletionPolicy(ctx, policyID, policy)
}

// WithNodeCompletionPolicy binds a reviewing verifier with an optional dispatch
// barrier and tool choice policy. A tool choice policy joins the continuation
// identity under the same policy ID, so a changed policy cannot resume a pause.
func WithNodeCompletionPolicy(ctx context.Context, policyID string, policy NodeCompletionPolicy) context.Context {
	return context.WithValue(ctx, nodeCompletionKey{}, nodeCompletionBinding{policyID: policyID, policy: policy})
}

func nodeCompletionPolicy(ctx context.Context) (string, NodeCompletionPolicy) {
	binding, _ := ctx.Value(nodeCompletionKey{}).(nodeCompletionBinding)
	return binding.policyID, binding.policy
}

// NodeCompletionCheckError is never a transport/retry signal. Its text contains
// only a bounded reason code, not a model answer or an underlying database error.
type NodeCompletionCheckError struct{ Reason string }

func (e *NodeCompletionCheckError) Error() string {
	return "declared completion check failed: " + e.Reason
}

// requiredToolNotCalledReason is reported when a round constrained by the
// declared check's tool choice still returned no matching call.
const requiredToolNotCalledReason = "required_business_action_not_called"
