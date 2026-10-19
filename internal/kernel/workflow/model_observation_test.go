package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

type modelObservationTestRecorder struct {
	mu      sync.Mutex
	events  []ModelCallEvent
	changed chan struct{}
}

func newModelObservationTestRecorder() *modelObservationTestRecorder {
	return &modelObservationTestRecorder{changed: make(chan struct{}, 1)}
}

func (recorder *modelObservationTestRecorder) observe(_ context.Context, event ModelCallEvent) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, event)
	recorder.mu.Unlock()
	select {
	case recorder.changed <- struct{}{}:
	default:
	}
}

func (recorder *modelObservationTestRecorder) snapshot() []ModelCallEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]ModelCallEvent(nil), recorder.events...)
}

func (recorder *modelObservationTestRecorder) wait(t *testing.T, phase ModelCallPhase) ModelCallEvent {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		for _, event := range recorder.snapshot() {
			if event.Phase == phase {
				return event
			}
		}
		select {
		case <-recorder.changed:
		case <-deadline.C:
			t.Fatalf("model call phase %q was not observed: %+v", phase, recorder.snapshot())
		}
	}
}

type modelObservationTestCloser struct{}

func (modelObservationTestCloser) Close() error { return nil }

type modelObservationTestFactory struct {
	buildCalls        int
	buildWithLLMCalls int
	llm               contract.LLM
}

func (factory *modelObservationTestFactory) Build(context.Context, frozen.FrozenExecutionBundle, RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
	factory.buildCalls++
	return compiler.FrozenBuildOpts{LLM: factory.llm}, modelObservationTestCloser{}, nil
}

func (factory *modelObservationTestFactory) BuildWithLLM(_ context.Context, _ frozen.FrozenExecutionBundle, _ RuntimeCredentialResolver, llm contract.LLM) (compiler.FrozenBuildOpts, io.Closer, error) {
	factory.buildWithLLMCalls++
	return compiler.FrozenBuildOpts{LLM: llm}, modelObservationTestCloser{}, nil
}

type modelObservationTestLLM struct {
	chat func(context.Context, contract.ChatRequest) (*contract.ChatResponse, error)
}

func (llm modelObservationTestLLM) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	if llm.chat == nil {
		return &contract.ChatResponse{Content: "completed"}, nil
	}
	return llm.chat(ctx, request)
}

func (modelObservationTestLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, errors.New("unused stream")
}

type modelObservationGatedStreamLLM struct {
	started     chan struct{}
	release     chan struct{}
	producerEnd chan struct{}
	firstChunk  contract.StreamChunk
	releaseOnce sync.Once
}

type modelObservationStreamErrorLLM struct{ err error }

func (llm modelObservationStreamErrorLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return nil, llm.err
}

func (llm modelObservationStreamErrorLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, llm.err
}

func newModelObservationGatedStreamLLM() *modelObservationGatedStreamLLM {
	return &modelObservationGatedStreamLLM{
		started: make(chan struct{}), release: make(chan struct{}), producerEnd: make(chan struct{}),
		firstChunk: contract.StreamChunk{Content: "held chunk"},
	}
}

func (llm *modelObservationGatedStreamLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return nil, errors.New("unused chat")
}

func (llm *modelObservationGatedStreamLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	stream := make(chan contract.StreamChunk)
	go func() {
		defer close(llm.producerEnd)
		defer close(stream)
		close(llm.started)
		stream <- llm.firstChunk
		<-llm.release // Deliberately ignore context cancellation until explicitly released.
	}()
	return stream, nil
}

func (llm *modelObservationGatedStreamLLM) releaseProducer() {
	llm.releaseOnce.Do(func() { close(llm.release) })
}

func buildObservedModelLLM(t *testing.T, inner contract.LLM, observer ModelCallObserver) contract.LLM {
	t.Helper()
	factory := RuntimeHostFactoryFunc(func(context.Context, frozen.FrozenExecutionBundle, RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		return compiler.FrozenBuildOpts{LLM: inner}, modelObservationTestCloser{}, nil
	})
	wrapped := ObserveRuntimeModels(factory, observer)
	opts, closer, err := wrapped.Build(context.Background(), frozen.FrozenExecutionBundle{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		t.Cleanup(func() { _ = closer.Close() })
	}
	if opts.LLM == nil {
		t.Fatal("model observer removed the frozen LLM")
	}
	return opts.LLM
}

func TestObserveRuntimeModelsReportsTransportAndStreamMilestonesBeforeChatReturns(t *testing.T) {
	requestReceived := make(chan struct{})
	writeHeaders := make(chan struct{})
	writeChunk := make(chan struct{})
	writeDone := make(chan struct{})
	serverDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer close(serverDone)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read controlled model request: %v", err)
			return
		}
		var decoded struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil || decoded.Model != "fixture-model" {
			t.Errorf("unexpected controlled model request: model=%q err=%v", decoded.Model, err)
		}
		close(requestReceived)
		select {
		case <-writeHeaders:
		case <-request.Context().Done():
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		select {
		case <-writeChunk:
		case <-request.Context().Done():
			return
		}
		_, _ = fmt.Fprint(writer, `data: {"id":"local","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"controlled response"},"finish_reason":null}]}`+"\n\n")
		writer.(http.Flusher).Flush()
		select {
		case <-writeDone:
		case <-request.Context().Done():
			return
		}
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"local\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		writer.(http.Flusher).Flush()
	}))
	defer server.Close()

	provider := llmrouter.NewProviderClient(llmrouter.ProviderConfig{Protocol: "openai-completions",
		ID: "controlled", BaseURL: server.URL, APIKey: "test-only-key", Models: []string{"fixture-model"},
		AttemptTimeoutSeconds: 10,
	})
	recorder := newModelObservationTestRecorder()
	model := buildObservedModelLLM(t, &runtimeStreamingLLM{inner: provider}, recorder.observe)
	var inheritedWrites, inheritedFirstBytes atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				inheritedWrites.Add(1)
			}
		},
		GotFirstResponseByte: func() { inheritedFirstBytes.Add(1) },
	})
	type chatResult struct {
		response *contract.ChatResponse
		err      error
	}
	finished := make(chan chatResult, 1)
	go func() {
		response, err := model.Chat(ctx, contract.ChatRequest{Model: "fixture-model", Messages: []contract.Message{{Role: "user", Content: "local test"}}})
		finished <- chatResult{response: response, err: err}
	}()
	started := recorder.wait(t, ModelCallStarted)
	if started.CallID == "" || started.Model != "fixture-model" || started.ElapsedMS < 0 {
		t.Fatalf("invalid started event: %+v", started)
	}
	select {
	case <-requestReceived:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled provider did not receive the local request")
	}
	requestWritten := recorder.wait(t, ModelCallRequestWritten)
	if requestWritten.CallID != started.CallID {
		t.Fatalf("request-written event changed identity: start=%+v written=%+v", started, requestWritten)
	}
	if len(recorder.snapshot()) != 2 {
		t.Fatalf("response milestones appeared before the response was released: %+v", recorder.snapshot())
	}
	close(writeHeaders)
	firstResponseByte := recorder.wait(t, ModelCallFirstResponseByte)
	if firstResponseByte.CallID != started.CallID {
		t.Fatalf("first-response-byte event changed identity: %+v", firstResponseByte)
	}
	select {
	case result := <-finished:
		t.Fatalf("chat returned before controlled first chunk and DONE: response=%+v err=%v", result.response, result.err)
	default:
	}
	close(writeChunk)
	firstChunk := recorder.wait(t, ModelCallFirstChunk)
	if firstChunk.CallID != started.CallID {
		t.Fatalf("first-chunk event changed identity: %+v", firstChunk)
	}
	select {
	case result := <-finished:
		t.Fatalf("chat returned before controlled DONE: response=%+v err=%v", result.response, result.err)
	default:
	}
	close(writeDone)
	select {
	case result := <-finished:
		if result.err != nil || result.response == nil || result.response.Content != "controlled response" {
			t.Fatalf("controlled chat failed: response=%+v err=%v", result.response, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chat did not return after controlled DONE")
	}
	completed := recorder.wait(t, ModelCallCompleted)
	if completed.CallID != started.CallID || completed.ErrorClass != "" {
		t.Fatalf("invalid completed event: %+v", completed)
	}
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("local provider handler did not finish")
	}
	if inheritedWrites.Load() != 1 || inheritedFirstBytes.Load() != 1 {
		t.Fatalf("provider HTTP trace was replaced instead of composed: wrote=%d first_byte=%d", inheritedWrites.Load(), inheritedFirstBytes.Load())
	}
	got := recorder.snapshot()
	want := []ModelCallPhase{ModelCallStarted, ModelCallRequestWritten, ModelCallFirstResponseByte, ModelCallFirstChunk, ModelCallCompleted}
	if len(got) != len(want) {
		t.Fatalf("model event count=%d, want=%d: %+v", len(got), len(want), got)
	}
	for index, phase := range want {
		if got[index].Phase != phase || got[index].CallID != started.CallID {
			t.Fatalf("model event[%d]=%+v, want phase=%s call_id=%s", index, got[index], phase, started.CallID)
		}
	}
}

func TestObserveRuntimeModelsStreamCancellationClosesObserverPump(t *testing.T) {
	requestReceived := make(chan struct{})
	serverStopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer close(serverStopped)
		_, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		_, _ = fmt.Fprint(writer, `data: {"id":"local","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		writer.(http.Flusher).Flush()
		close(requestReceived)
		<-request.Context().Done()
	}))
	defer server.Close()
	provider := llmrouter.NewProviderClient(llmrouter.ProviderConfig{Protocol: "openai-completions",
		ID: "controlled", BaseURL: server.URL, APIKey: "test-only-key", Models: []string{"fixture-model"},
		AttemptTimeoutSeconds: 10,
	})
	recorder := newModelObservationTestRecorder()
	model := buildObservedModelLLM(t, provider, recorder.observe)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := model.Stream(ctx, contract.ChatRequest{Model: "fixture-model", Messages: []contract.Message{{Role: "user", Content: "local cancellation test"}}})
	if err != nil || stream == nil {
		cancel()
		t.Fatalf("start controlled stream: stream=%v err=%v", stream, err)
	}
	started := recorder.wait(t, ModelCallStarted)
	_ = recorder.wait(t, ModelCallRequestWritten)
	_ = recorder.wait(t, ModelCallFirstResponseByte)
	select {
	case <-requestReceived:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("controlled stream did not reach local handler")
	}
	select {
	case chunk, open := <-stream:
		if !open || chunk.Content != "partial" {
			cancel()
			t.Fatalf("controlled stream first chunk: open=%v chunk=%+v", open, chunk)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("controlled stream did not yield its first chunk")
	}
	_ = recorder.wait(t, ModelCallFirstChunk)
	cancel()
	for range stream {
	}
	select {
	case <-serverStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("request context did not cancel the local provider handler")
	}
	cancelled := recorder.wait(t, ModelCallCancelled)
	if cancelled.CallID != started.CallID || cancelled.ErrorClass != ModelCallErrorContextCancelled {
		t.Fatalf("invalid cancellation event: %+v", cancelled)
	}
	for _, event := range recorder.snapshot() {
		if event.Phase == ModelCallCompleted {
			t.Fatalf("cancelled stream was also reported completed: %+v", recorder.snapshot())
		}
	}
}

func TestStreamWrappersReturnNilStreamErrorWithoutBlocking(t *testing.T) {
	sentinel := errors.New("controlled stream setup failure")
	source := modelObservationStreamErrorLLM{err: sentinel}
	request := contract.ChatRequest{Model: "fixture-model"}

	t.Run("runtime streaming Chat", func(t *testing.T) {
		type result struct {
			err error
		}
		finished := make(chan result, 1)
		go func() {
			_, err := (&runtimeStreamingLLM{inner: source}).Chat(context.Background(), request)
			finished <- result{err: err}
		}()
		select {
		case got := <-finished:
			if !errors.Is(got.err, sentinel) {
				t.Fatalf("runtime streaming Chat changed the source error: %v", got.err)
			}
		case <-time.After(time.Second):
			t.Fatal("runtime streaming Chat blocked draining a nil stream")
		}
	})

	t.Run("observed Stream", func(t *testing.T) {
		model := buildObservedModelLLM(t, source, func(context.Context, ModelCallEvent) {})
		type result struct {
			stream <-chan contract.StreamChunk
			err    error
		}
		finished := make(chan result, 1)
		go func() {
			stream, err := model.Stream(context.Background(), request)
			finished <- result{stream: stream, err: err}
		}()
		select {
		case got := <-finished:
			if got.stream != nil || !errors.Is(got.err, sentinel) {
				t.Fatalf("observed Stream changed nil-stream error: stream=%v err=%v", got.stream, got.err)
			}
		case <-time.After(time.Second):
			t.Fatal("observed Stream blocked draining a nil stream")
		}
	})
}

func TestRuntimeStreamingChatWaitsForIgnoredCancellationUntilUpstreamCloses(t *testing.T) {
	source := newModelObservationGatedStreamLLM()
	defer source.releaseProducer()
	model := &runtimeStreamingLLM{inner: source}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, err := model.Chat(ctx, contract.ChatRequest{Model: "fixture-model"})
		finished <- err
	}()
	select {
	case <-source.started:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled upstream stream did not start")
	}
	cancel()
	select {
	case err := <-finished:
		t.Fatalf("Chat returned before ignored-cancellation upstream closed: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	source.releaseProducer()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Chat did not preserve caller cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Chat did not return after upstream stream closed")
	}
	select {
	case <-source.producerEnd:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream producer did not exit")
	}
}

func TestObservedStreamWaitsForIgnoredCancellationUntilUpstreamCloses(t *testing.T) {
	source := newModelObservationGatedStreamLLM()
	defer source.releaseProducer()
	recorder := newModelObservationTestRecorder()
	model := buildObservedModelLLM(t, source, recorder.observe)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := model.Stream(ctx, contract.ChatRequest{Model: "fixture-model"})
	if err != nil || stream == nil {
		cancel()
		t.Fatalf("start controlled stream: stream=%v err=%v", stream, err)
	}
	started := recorder.wait(t, ModelCallStarted)
	select {
	case <-source.started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("controlled upstream stream did not start")
	}
	select {
	case chunk, open := <-stream:
		if !open || chunk.Content != "held chunk" {
			cancel()
			t.Fatalf("controlled stream chunk: open=%v chunk=%+v", open, chunk)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("controlled stream did not yield its first chunk")
	}
	_ = recorder.wait(t, ModelCallFirstChunk)
	cancel()
	select {
	case chunk, open := <-stream:
		if !open {
			t.Fatal("observed output closed before upstream producer exited")
		}
		t.Fatalf("observed output forwarded a post-cancellation chunk: %+v", chunk)
	case <-time.After(40 * time.Millisecond):
	}
	for _, event := range recorder.snapshot() {
		if event.Phase == ModelCallCancelled || event.Phase == ModelCallCompleted || event.Phase == ModelCallFailed {
			t.Fatalf("terminal event was emitted before upstream closed: %+v", recorder.snapshot())
		}
	}
	source.releaseProducer()
	for range stream {
	}
	select {
	case <-source.producerEnd:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream producer did not exit")
	}
	cancelled := recorder.wait(t, ModelCallCancelled)
	if cancelled.CallID != started.CallID || cancelled.ErrorClass != ModelCallErrorContextCancelled {
		t.Fatalf("invalid post-join cancellation event: %+v", cancelled)
	}
}

func TestObservedStreamWaitsForUpstreamCloseAfterDone(t *testing.T) {
	source := newModelObservationGatedStreamLLM()
	source.firstChunk = contract.StreamChunk{Content: "final chunk", Done: true}
	defer source.releaseProducer()
	recorder := newModelObservationTestRecorder()
	model := buildObservedModelLLM(t, source, recorder.observe)
	stream, err := model.Stream(context.Background(), contract.ChatRequest{Model: "fixture-model"})
	if err != nil || stream == nil {
		t.Fatalf("start controlled stream: stream=%v err=%v", stream, err)
	}
	started := recorder.wait(t, ModelCallStarted)
	select {
	case <-source.started:
	case <-time.After(2 * time.Second):
		t.Fatal("controlled upstream stream did not start")
	}
	select {
	case chunk, open := <-stream:
		if !open || !chunk.Done || chunk.Content != "final chunk" {
			t.Fatalf("controlled done chunk: open=%v chunk=%+v", open, chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controlled stream did not yield its done chunk")
	}
	_ = recorder.wait(t, ModelCallFirstChunk)
	quietWindow := time.NewTimer(40 * time.Millisecond)
	defer quietWindow.Stop()
quiet:
	for {
		select {
		case <-recorder.changed:
		case <-quietWindow.C:
			break quiet
		}
	}
	for _, event := range recorder.snapshot() {
		if event.Phase == ModelCallCompleted || event.Phase == ModelCallFailed || event.Phase == ModelCallCancelled {
			t.Fatalf("terminal event was emitted before upstream channel closed: %+v", recorder.snapshot())
		}
	}
	select {
	case _, open := <-stream:
		if !open {
			t.Fatal("observed output closed before upstream producer exited")
		}
		t.Fatal("unexpected post-Done model chunk")
	case <-time.After(20 * time.Millisecond):
	}
	source.releaseProducer()
	for range stream {
	}
	select {
	case <-source.producerEnd:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream producer did not exit")
	}
	completed := recorder.wait(t, ModelCallCompleted)
	if completed.CallID != started.CallID || completed.ErrorClass != "" {
		t.Fatalf("invalid completion event after upstream close: %+v", completed)
	}
}

func TestObserveRuntimeModelsPreservesBuildWithLLMAndModelError(t *testing.T) {
	providerErr := errors.New("test-only provider error with private response text")
	innerLLM := modelObservationTestLLM{chat: func(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
		return nil, providerErr
	}}
	factory := &modelObservationTestFactory{}
	recorder := newModelObservationTestRecorder()
	decorated := ObserveRuntimeModels(factory, recorder.observe)
	withLLM, ok := decorated.(RuntimeHostFactoryWithLLM)
	if !ok {
		t.Fatal("model observer removed BuildWithLLM capability")
	}
	options, closer, err := withLLM.BuildWithLLM(context.Background(), frozen.FrozenExecutionBundle{}, nil, innerLLM)
	if err != nil {
		t.Fatal(err)
	}
	if closer != factoryCloserForTest {
		// BuildWithLLM must preserve the inner resource owner returned by the factory.
		t.Fatal("model observer replaced the BuildWithLLM closer")
	}
	_, err = options.LLM.Chat(context.Background(), contract.ChatRequest{Model: "fixture-model"})
	if !errors.Is(err, providerErr) {
		t.Fatalf("observer changed the model error: %v", err)
	}
	failed := recorder.wait(t, ModelCallFailed)
	if failed.ErrorClass != ModelCallErrorProvider || failed.CallID == "" {
		t.Fatalf("invalid failed event: %+v", failed)
	}
	encoded, _ := json.Marshal(recorder.snapshot())
	if strings.Contains(string(encoded), "private response text") || strings.Contains(string(encoded), providerErr.Error()) {
		t.Fatal("model observation retained raw provider error text")
	}
	if factory.buildWithLLMCalls != 1 || factory.buildCalls != 0 {
		t.Fatalf("BuildWithLLM dispatch changed: build=%d build_with_llm=%d", factory.buildCalls, factory.buildWithLLMCalls)
	}
}

func TestObserveRuntimeModelsObserverPanicDoesNotChangeModelResult(t *testing.T) {
	providerErr := errors.New("test-only provider error")
	model := buildObservedModelLLM(t, modelObservationTestLLM{chat: func(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
		return nil, providerErr
	}}, func(context.Context, ModelCallEvent) {
		panic("observer failure")
	})
	_, err := model.Chat(context.Background(), contract.ChatRequest{Model: "fixture-model"})
	if !errors.Is(err, providerErr) {
		t.Fatalf("observer panic changed the model error: %v", err)
	}
}

var factoryCloserForTest io.Closer = modelObservationTestCloser{}

func TestObserveRuntimeModelsErrorClassificationIsBounded(t *testing.T) {
	deadline := &net.OpError{Op: "dial", Net: "tcp", Err: context.DeadlineExceeded}
	transport := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	for _, test := range []struct {
		name string
		ctx  context.Context
		err  error
		want ModelCallErrorClass
	}{
		{name: "context deadline", err: context.DeadlineExceeded, want: ModelCallErrorContextDeadline},
		{name: "context cancellation", err: context.Canceled, want: ModelCallErrorContextCancelled},
		{name: "transport deadline", err: deadline, want: ModelCallErrorContextDeadline},
		{name: "transport", err: transport, want: ModelCallErrorTransport},
		{name: "provider", err: errors.New("HTTP 502"), want: ModelCallErrorProvider},
		{name: "unknown", want: ModelCallErrorUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyModelCallError(test.ctx, test.err); got != test.want {
				t.Fatalf("error class=%q, want=%q", got, test.want)
			}
		})
	}
}
