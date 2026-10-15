package workflow

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
)

type ModelCallPhase string

const (
	ModelCallStarted           ModelCallPhase = "started"
	ModelCallRequestWritten    ModelCallPhase = "request_written"
	ModelCallFirstResponseByte ModelCallPhase = "first_response_byte"
	ModelCallFirstChunk        ModelCallPhase = "first_chunk"
	ModelCallCompleted         ModelCallPhase = "completed"
	ModelCallFailed            ModelCallPhase = "failed"
	ModelCallCancelled         ModelCallPhase = "cancelled"
)

type ModelCallErrorClass string

const (
	ModelCallErrorContextDeadline  ModelCallErrorClass = "context_deadline"
	ModelCallErrorContextCancelled ModelCallErrorClass = "context_cancelled"
	ModelCallErrorTransport        ModelCallErrorClass = "transport"
	ModelCallErrorProvider         ModelCallErrorClass = "provider"
	ModelCallErrorUnknown          ModelCallErrorClass = "unknown"
)

// ModelCallEvent contains only safe correlation and lifecycle facts. It never
// includes request headers, prompts, model output, or raw errors.
type ModelCallEvent struct {
	CallID     string              `json:"call_id"`
	Model      string              `json:"model,omitempty"`
	Phase      ModelCallPhase      `json:"phase"`
	ElapsedMS  int64               `json:"elapsed_ms"`
	ErrorClass ModelCallErrorClass `json:"error_class,omitempty"`
}

// ModelCallObserver should record the bounded event promptly. Panics are
// isolated so observation cannot change model behavior.
type ModelCallObserver func(context.Context, ModelCallEvent)

// ObserveRuntimeModels adds model lifecycle observation to each frozen runtime
// host while preserving BuildWithLLM on factories that provide it.
func ObserveRuntimeModels(inner RuntimeHostFactory, observer ModelCallObserver) RuntimeHostFactory {
	if inner == nil || observer == nil {
		return inner
	}
	observed := observedModelRuntimeHostFactory{inner: inner, observer: observer}
	if withLLM, ok := inner.(RuntimeHostFactoryWithLLM); ok {
		return observedModelRuntimeHostFactoryWithLLM{
			observedModelRuntimeHostFactory: observed,
			innerWithLLM:                    withLLM,
		}
	}
	return observed
}

type observedModelRuntimeHostFactory struct {
	inner    RuntimeHostFactory
	observer ModelCallObserver
}

func (f observedModelRuntimeHostFactory) Build(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	resolver RuntimeCredentialResolver,
) (compiler.FrozenBuildOpts, io.Closer, error) {
	opts, closer, err := f.inner.Build(ctx, bundle, resolver)
	return f.observe(opts, closer, err)
}

func (f observedModelRuntimeHostFactory) observe(
	opts compiler.FrozenBuildOpts,
	closer io.Closer,
	err error,
) (compiler.FrozenBuildOpts, io.Closer, error) {
	if err == nil && opts.LLM != nil {
		opts.LLM = &observedRuntimeModelLLM{inner: opts.LLM, observer: f.observer}
	}
	return opts, closer, err
}

type observedModelRuntimeHostFactoryWithLLM struct {
	observedModelRuntimeHostFactory
	innerWithLLM RuntimeHostFactoryWithLLM
}

func (f observedModelRuntimeHostFactoryWithLLM) BuildWithLLM(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	resolver RuntimeCredentialResolver,
	llm contract.LLM,
) (compiler.FrozenBuildOpts, io.Closer, error) {
	opts, closer, err := f.innerWithLLM.BuildWithLLM(ctx, bundle, resolver, llm)
	return f.observe(opts, closer, err)
}

type observedRuntimeModelLLM struct {
	inner    contract.LLM
	observer ModelCallObserver
}

func (llm *observedRuntimeModelLLM) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	if llm == nil || llm.inner == nil {
		return nil, errors.New("workflow model observer has no underlying LLM")
	}
	call := newModelCall(ctx, request.Model, llm.observer)
	call.emit(ctx, ModelCallStarted, "")
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	response, err := llm.inner.Chat(call.withHTTPTrace(callCtx), request)
	call.finish(ctx, err)
	return response, err
}

func (llm *observedRuntimeModelLLM) Stream(ctx context.Context, request contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	if llm == nil || llm.inner == nil {
		return nil, errors.New("workflow model observer has no underlying LLM")
	}
	call := newModelCall(ctx, request.Model, llm.observer)
	call.emit(ctx, ModelCallStarted, "")
	callCtx, cancel := context.WithCancel(ctx)
	stream, err := llm.inner.Stream(call.withHTTPTrace(callCtx), request)
	if err != nil {
		cancel()
		if stream != nil {
			for range stream {
			}
		}
		call.finish(ctx, err)
		return stream, err
	}
	if stream == nil {
		cancel()
		call.finishUnexpected(ctx)
		return nil, nil
	}

	out := make(chan contract.StreamChunk, 32)
	go func() {
		defer close(out)
		defer cancel()
		forward := true
		cancelled := false
		done := false
		var streamErr error
		for {
			if !forward {
				if _, open := <-stream; !open {
					break
				}
				continue
			}
			if ctx.Err() != nil {
				cancelled = true
				forward = false
				cancel()
				continue
			}
			select {
			case <-ctx.Done():
				cancelled = true
				forward = false
				cancel()
			case chunk, open := <-stream:
				if !open {
					if streamErr != nil {
						call.finish(ctx, streamErr)
					} else if cancelled {
						call.finish(ctx, ctx.Err())
					} else if done {
						call.finish(ctx, nil)
					} else {
						call.finishUnexpected(ctx)
					}
					return
				}
				if chunk.Err != nil {
					streamErr = chunk.Err
					forward = false
					cancel()
					if ctx.Err() != nil {
						cancelled = true
						continue
					}
					select {
					case out <- chunk:
					case <-ctx.Done():
						cancelled = true
					}
					continue
				}
				call.firstChunk(ctx)
				if chunk.Done {
					done = true
				}
				if ctx.Err() != nil {
					cancelled = true
					forward = false
					cancel()
					continue
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					cancelled = true
					forward = false
					cancel()
				}
			}
		}
		if streamErr != nil {
			call.finish(ctx, streamErr)
		} else if cancelled {
			call.finish(ctx, ctx.Err())
		} else if done {
			call.finish(ctx, nil)
		} else {
			call.finishUnexpected(ctx)
		}
	}()
	return out, nil
}

type runtimeModelCallContextKey struct{}

type runtimeModelCall struct {
	callID   string
	model    string
	started  time.Time
	observer ModelCallObserver

	mu       sync.Mutex
	emitted  map[ModelCallPhase]struct{}
	terminal bool
}

func newModelCall(_ context.Context, model string, observer ModelCallObserver) *runtimeModelCall {
	return &runtimeModelCall{
		callID: uuid.NewString(), model: model, started: time.Now(), observer: observer,
		emitted: make(map[ModelCallPhase]struct{}, 6),
	}
}

func (call *runtimeModelCall) withHTTPTrace(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, runtimeModelCallContextKey{}, call)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				call.emit(ctx, ModelCallRequestWritten, "")
			}
		},
		GotFirstResponseByte: func() {
			call.emit(ctx, ModelCallFirstResponseByte, "")
		},
	})
}

func notifyRuntimeModelFirstChunk(ctx context.Context) {
	call, _ := ctx.Value(runtimeModelCallContextKey{}).(*runtimeModelCall)
	if call != nil {
		call.firstChunk(ctx)
	}
}

func (call *runtimeModelCall) firstChunk(ctx context.Context) {
	call.emit(ctx, ModelCallFirstChunk, "")
}

func (call *runtimeModelCall) finish(ctx context.Context, err error) {
	if err == nil {
		call.emit(ctx, ModelCallCompleted, "")
		return
	}
	class := classifyModelCallError(ctx, err)
	phase := ModelCallFailed
	if class == ModelCallErrorContextCancelled {
		phase = ModelCallCancelled
	}
	call.emit(ctx, phase, class)
}

func (call *runtimeModelCall) finishUnexpected(ctx context.Context) {
	call.emit(ctx, ModelCallFailed, ModelCallErrorUnknown)
}

func (call *runtimeModelCall) emit(ctx context.Context, phase ModelCallPhase, errorClass ModelCallErrorClass) {
	if call == nil || call.observer == nil {
		return
	}
	call.mu.Lock()
	if call.terminal {
		call.mu.Unlock()
		return
	}
	if _, exists := call.emitted[phase]; exists {
		call.mu.Unlock()
		return
	}
	call.emitted[phase] = struct{}{}
	if phase == ModelCallCompleted || phase == ModelCallFailed || phase == ModelCallCancelled {
		call.terminal = true
	}
	event := ModelCallEvent{
		CallID: call.callID, Model: call.model, Phase: phase,
		ElapsedMS: time.Since(call.started).Milliseconds(), ErrorClass: errorClass,
	}
	call.mu.Unlock()
	defer func() { _ = recover() }()
	call.observer(ctx, event)
}

func classifyModelCallError(ctx context.Context, err error) ModelCallErrorClass {
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return ModelCallErrorContextDeadline
	}
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return ModelCallErrorContextCancelled
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		if networkError.Timeout() {
			return ModelCallErrorContextDeadline
		}
		return ModelCallErrorTransport
	}
	if err == nil {
		return ModelCallErrorUnknown
	}
	return ModelCallErrorProvider
}
