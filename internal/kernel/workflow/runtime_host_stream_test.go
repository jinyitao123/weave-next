package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

type hostStreamFixture struct {
	chunks           []contract.StreamChunk
	chatCalls        int
	streamCtx        context.Context
	started          chan struct{}
	stopped          chan struct{}
	cancelDuringChat context.CancelFunc
}

func (f *hostStreamFixture) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	f.chatCalls++
	if f.cancelDuringChat != nil {
		f.cancelDuringChat()
	}
	return &contract.ChatResponse{Content: "fallback", StopReason: "stop"}, nil
}

func (f *hostStreamFixture) Stream(ctx context.Context, _ contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	f.streamCtx = ctx
	if f.started != nil {
		ch := make(chan contract.StreamChunk)
		close(f.started)
		go func() {
			<-ctx.Done()
			close(ch)
			close(f.stopped)
		}()
		return ch, nil
	}
	ch := make(chan contract.StreamChunk, len(f.chunks))
	for _, chunk := range f.chunks {
		ch <- chunk
	}
	close(ch)
	return ch, nil
}

func TestRuntimeStreamPreservesStopReasonAndToolCalls(t *testing.T) {
	fixture := &hostStreamFixture{chunks: []contract.StreamChunk{
		{Content: "checking", FinishReason: "tool_calls"},
		{ToolCalls: []contract.ToolCall{{ID: "call-1", Name: "lookup", Args: `{}`}}},
		{Done: true, Usage: &contract.Usage{OutputTokens: 12}},
	}}
	response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(t.Context(), contract.ChatRequest{})
	if err != nil || response == nil || response.StopReason != "tool_calls" || response.Content != "checking" ||
		len(response.ToolCalls) != 1 || response.ToolCalls[0].Name != "lookup" || response.Usage.OutputTokens != 12 {
		t.Fatalf("stream facts lost: response=%+v err=%v", response, err)
	}
	if fixture.chatCalls != 0 || fixture.streamCtx.Err() != context.Canceled {
		t.Fatalf("unexpected fallback or stream lifetime: fallback=%d err=%v", fixture.chatCalls, fixture.streamCtx.Err())
	}
}

func TestRuntimeStreamDoesNotCompleteOrRetryAfterPartialFailure(t *testing.T) {
	for _, prefix := range []contract.StreamChunk{
		{}, {Content: `{"answer":"looks complete"}`},
		{ToolCalls: []contract.ToolCall{{ID: "call-1", Name: "update", Args: `{}`}}},
	} {
		for _, streamErr := range []error{openai.ErrStreamClosedWithoutDone, context.Canceled, errors.New("provider rejected stream")} {
			fixture := &hostStreamFixture{chunks: []contract.StreamChunk{prefix, {Err: streamErr, Done: true}}}
			response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(t.Context(), contract.ChatRequest{})
			if response != nil || !errors.Is(err, streamErr) || fixture.chatCalls != 0 {
				t.Fatalf("partial stream accepted or replayed: response=%+v err=%v fallback=%d", response, err, fixture.chatCalls)
			}
			if fixture.streamCtx.Err() != context.Canceled {
				t.Fatal("failed stream was not cancelled")
			}
		}
	}
}

func TestRuntimeStreamRejectsMissingDoneAndCancelledFallback(t *testing.T) {
	fixture := &hostStreamFixture{chunks: []contract.StreamChunk{{Content: `{"answer":"partial"}`}}}
	response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(t.Context(), contract.ChatRequest{})
	if response != nil || err == nil || fixture.chatCalls != 0 {
		t.Fatalf("unterminated stream accepted: response=%+v err=%v", response, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fixture = &hostStreamFixture{}
	response, err = (&runtimeStreamingLLM{inner: fixture}).Chat(ctx, contract.ChatRequest{})
	if response != nil || !errors.Is(err, context.Canceled) || fixture.chatCalls != 0 {
		t.Fatalf("cancelled stream replayed: response=%+v err=%v fallback=%d", response, err, fixture.chatCalls)
	}
}

func TestRuntimeStreamRetainsEmptyUnsupportedStreamFallback(t *testing.T) {
	fixture := &hostStreamFixture{}
	response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(t.Context(), contract.ChatRequest{})
	if err != nil || response == nil || response.Content != "fallback" || response.StopReason != "stop" || fixture.chatCalls != 1 {
		t.Fatalf("empty-stream fallback changed: response=%+v err=%v fallback=%d", response, err, fixture.chatCalls)
	}
}

func TestRuntimeStreamRejectsCancellationDuringFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture := &hostStreamFixture{cancelDuringChat: cancel}
	response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(ctx, contract.ChatRequest{})
	if response != nil || !errors.Is(err, context.Canceled) || fixture.chatCalls != 1 {
		t.Fatalf("cancelled fallback accepted: response=%+v err=%v fallback=%d", response, err, fixture.chatCalls)
	}
}

func TestRuntimeStreamDoesNotFallbackAfterMetadataOnlyFrame(t *testing.T) {
	for _, chunk := range []contract.StreamChunk{{FinishReason: "length"}, {Usage: &contract.Usage{}}, {}} {
		fixture := &hostStreamFixture{chunks: []contract.StreamChunk{chunk}}
		response, err := (&runtimeStreamingLLM{inner: fixture}).Chat(t.Context(), contract.ChatRequest{})
		if response != nil || err == nil || fixture.chatCalls != 0 {
			t.Fatalf("metadata-only stream replayed: response=%+v err=%v fallback=%d", response, err, fixture.chatCalls)
		}
	}
}

func TestRuntimeStreamCancellationStopsWaitingAndProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture := &hostStreamFixture{started: make(chan struct{}), stopped: make(chan struct{})}
	finished := make(chan error, 1)
	go func() {
		_, err := (&runtimeStreamingLLM{inner: fixture}).Chat(ctx, contract.ChatRequest{})
		finished <- err
	}()
	select {
	case <-fixture.started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || fixture.chatCalls != 0 {
			t.Fatalf("cancelled stream returned success or replayed: err=%v fallback=%d", err, fixture.chatCalls)
		}
	case <-time.After(time.Second):
		t.Fatal("stream wrapper did not stop waiting")
	}
	select {
	case <-fixture.stopped:
	case <-time.After(time.Second):
		t.Fatal("stream producer did not exit")
	}
}
