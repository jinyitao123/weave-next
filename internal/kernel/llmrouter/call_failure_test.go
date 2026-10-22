package llmrouter

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

func TestDiagnoseCallErrorUsesTypedEvidenceOnly(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		class  string
		status int
		finish string
	}{
		{"System One rate limit", fmt.Errorf("decide: %w", &systemOneHTTPError{status: 429}), CallFailureRateLimited, 429, ""},
		{"wrapped OpenAI 503", fmt.Errorf("attempt: %w", fmt.Errorf("route: %w", &openai.HTTPError{StatusCode: 503})), CallFailureUpstream5xx, 503, ""},
		{"not found model", &openai.HTTPError{StatusCode: 404}, CallFailureUnsupportedRequest, 404, ""},
		{"refused before sending", fmt.Errorf("chat: %w", openai.ErrJSONModeInstructionMissing), CallFailureUnsupportedRequest, 0, ""},
		{"non-stream empty", &openai.EmptyResponseError{FinishReason: "content_filter"}, CallFailureEmptyResponse, 0, "content_filter"},
		{"unknown finish reason is dropped", &openai.EmptyResponseError{FinishReason: "secret provider text"}, CallFailureEmptyResponse, 0, ""},
		{"stream closed early", &openai.EmptyResponseError{Stream: true, CloseKind: "eof"}, CallFailureStreamTruncated, 0, ""},
		{"no typed evidence", errors.New("HTTP 429 in some text"), "", 0, ""},
	}
	for _, tc := range cases {
		failure := DiagnoseCallError(tc.err)
		if failure.Class != tc.class || failure.HTTPStatus != tc.status || failure.FinishReason != tc.finish {
			t.Errorf("%s: %+v, want class %q status %d finish %q", tc.name, failure, tc.class, tc.status, tc.finish)
		}
	}
	if failure := DiagnoseCallError(nil); failure != (CallFailure{}) {
		t.Errorf("nil error diagnosed as %+v", failure)
	}
}

// countingLLM fails every call with err and counts the calls per model.
type countingLLM struct {
	err   error
	calls map[string]int
}

func (c *countingLLM) Chat(_ context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	c.calls[req.Model]++
	return nil, c.err
}

func (c *countingLLM) Stream(_ context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	c.calls[req.Model]++
	return nil, c.err
}

func TestFallbackNeverRepeatsACallEndedByItsWaitBudget(t *testing.T) {
	timeout := fmt.Errorf("attempt: %w", &openai.StageTimeoutError{Stage: "first_chunk", Budget: time.Second})
	if failure := DiagnoseCallError(timeout); failure.Class != CallFailureStageTimeout || failure.Stage != "first_chunk" || failure.Retryable != nil {
		t.Fatalf("stage timeout diagnosed as %+v", failure)
	}
	for _, stream := range []bool{false, true} {
		inner := &countingLLM{err: timeout, calls: map[string]int{}}
		llm := NewFallbackLLM(inner, "primary", []string{"backup"}, 2)
		var err error
		if stream {
			_, err = llm.Stream(t.Context(), contract.ChatRequest{Model: "primary"})
		} else {
			_, err = llm.Chat(t.Context(), contract.ChatRequest{Model: "primary"})
		}
		if !errors.Is(err, openai.ErrStageTimeout) || inner.calls["primary"] != 1 || inner.calls["backup"] != 0 {
			t.Fatalf("stream=%v: err=%v calls=%v; want one primary call and the timeout", stream, err, inner.calls)
		}
	}
}
