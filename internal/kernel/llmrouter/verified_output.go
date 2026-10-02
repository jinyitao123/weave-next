package llmrouter

import (
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/jinyitao123/loom/contract"
)

type locallyVerifiedOutputKey struct{}
type locallyVerifiedOutput struct {
	digest [sha256.Size]byte
	bound  bool
}

// WithLocallyVerifiedOutput is a host assertion that the exact final schema is
// already enforced locally and included in the model's instructions. It is not
// accepted from model state or a request field. Empty schema clears inheritance.
func WithLocallyVerifiedOutput(ctx context.Context, schema json.RawMessage) context.Context {
	return context.WithValue(ctx, locallyVerifiedOutputKey{}, locallyVerifiedOutput{
		digest: sha256.Sum256(schema), bound: len(schema) > 0,
	})
}

// This wrapper sits at the provider leaf, below usage and execution journals.
// The logical request retains its schema; only this wire request copy omits
// json_object while tools are offered. Other provider profiles are not wrapped.
type locallyVerifiedJSONObjectLLM struct{ inner contract.LLM }

func locallyVerifiedToolRequest(ctx context.Context, request contract.ChatRequest) contract.ChatRequest {
	verified, ok := ctx.Value(locallyVerifiedOutputKey{}).(locallyVerifiedOutput)
	if ok && verified.bound && request.Schema != nil && len(request.Tools) > 0 && verified.digest == sha256.Sum256(*request.Schema) {
		request.Schema = nil
	}
	return request
}

func (llm *locallyVerifiedJSONObjectLLM) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	return llm.inner.Chat(ctx, locallyVerifiedToolRequest(ctx, request))
}

func (llm *locallyVerifiedJSONObjectLLM) Stream(ctx context.Context, request contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return llm.inner.Stream(ctx, locallyVerifiedToolRequest(ctx, request))
}
