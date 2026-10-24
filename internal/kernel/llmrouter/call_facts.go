package llmrouter

import (
	"context"

	"github.com/jinyitao123/loom/contract"
)

// CallFacts is what the provider boundary observed about one physical model
// request: what went on the wire and what came back, enough to file a provider
// ticket (#108). It holds enums, counts and the provider's own short request
// ID; never messages, headers, credentials, response text or reasoning. A
// zero or empty field means not observed, except where a pointer is used to
// tell an observed zero from no observation.
type CallFacts struct {
	// HTTPStatus is the response status, 200 included; zero means no response.
	HTTPStatus int `json:"http_status,omitempty"`
	// ProviderRequestID comes from an allowlisted response header only.
	ProviderRequestID string `json:"provider_request_id,omitempty"`
	// End is how the protocol exchange ended, as the adapter saw it:
	// response or done for an answer that arrived whole; empty_response for
	// a whole reply with no answer; eof or scanner for a stream cut short;
	// api_error, http_error, decode_error or read_error for a refused or
	// unreadable reply; transport_error when no response arrived; cancel,
	// stage_timeout, or before_send.
	End string `json:"end,omitempty"`
	// FinishReason is the provider's stop reason when it gave a known one.
	FinishReason string `json:"finish_reason,omitempty"`
	// DoneSeen reports the stream's [DONE] marker.
	DoneSeen bool `json:"done_seen,omitempty"`
	// ContentBytes and ReasoningBytes count answer and reasoning text bytes
	// received; nil means not counted.
	ContentBytes   *int         `json:"content_bytes,omitempty"`
	ReasoningBytes *int         `json:"reasoning_bytes,omitempty"`
	Request        *CallRequest `json:"request,omitempty"`
	RequestBytes   int          `json:"request_bytes,omitempty"`
}

// CallRequest is the request as written to the wire, by enum and number.
// OutputLimitField names the provider's per-response limit field
// (max_tokens or max_completion_tokens), distinct from a member's cumulative
// token budget.
type CallRequest struct {
	Model            string `json:"model,omitempty"`
	Stream           bool   `json:"stream,omitempty"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	Thinking         string `json:"thinking,omitempty"`
	ResponseFormat   string `json:"response_format,omitempty"`
	OutputLimitField string `json:"output_limit_field,omitempty"`
	OutputLimit      int    `json:"output_limit,omitempty"`
}

type callFactsKey struct{}

func callFactsObserverFrom(ctx context.Context) func(CallFacts) {
	observe, _ := ctx.Value(callFactsKey{}).(func(CallFacts))
	return observe
}

func callFactsFrom(value contract.ProtocolObservation) CallFacts {
	facts := CallFacts{HTTPStatus: value.HTTPStatus, ProviderRequestID: value.ProviderRequestID,
		End: value.End, DoneSeen: value.DoneSeen, RequestBytes: value.RequestBytes}
	if value.FinishReason != "unknown" {
		facts.FinishReason = value.FinishReason
	}
	if value.Content != nil {
		contentBytes, reasoningBytes := value.Content.ContentBytes, value.Content.ReasoningBytes
		facts.ContentBytes, facts.ReasoningBytes = &contentBytes, &reasoningBytes
	}
	if options := value.RequestOptions; options != nil {
		facts.Request = &CallRequest{Model: options.Model, Stream: options.Stream, ReasoningEffort: options.ReasoningEffort,
			Thinking: options.Thinking, ResponseFormat: options.ResponseFormat,
			OutputLimitField: options.OutputLimit, OutputLimit: options.OutputLimitValue}
	}
	return facts
}
