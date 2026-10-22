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
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
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
	// Phases is set only on a compact terminal event: the relative time of each
	// intermediate phase that arrived, whether or not it was also emitted alone.
	Phases        map[ModelCallPhase]int64 `json:"phases_ms,omitempty"`
	GatewayWaitMS int64                    `json:"gateway_wait_ms,omitempty"`
}

// ModelCallObserver should record the bounded event promptly. Panics are
// isolated so observation cannot change model behavior.
type ModelCallObserver func(context.Context, ModelCallEvent)

// ModelObservationOptions changes how many events one call produces, never
// which facts are observed.
type ModelObservationOptions struct {
	// CompactPhases emits only started and the terminal event, folding the
	// intermediate phases into the terminal event's Phases. A call still open
	// after SlowAfter emits its arrived phases separately, and later ones as
	// they arrive, so a hung call stays diagnosable. SlowAfter <= 0 never does.
	CompactPhases bool
	SlowAfter     time.Duration
}

// ObserveRuntimeModels adds model lifecycle observation to each frozen runtime
// host while preserving BuildWithLLM on factories that provide it.
func ObserveRuntimeModels(inner RuntimeHostFactory, observer ModelCallObserver) RuntimeHostFactory {
	return ObserveRuntimeModelsWithOptions(inner, observer, ModelObservationOptions{})
}

// ObserveRuntimeModelsWithOptions is ObserveRuntimeModels with an explicit
// event shape; the zero options keep one event per phase.
func ObserveRuntimeModelsWithOptions(inner RuntimeHostFactory, observer ModelCallObserver, options ModelObservationOptions) RuntimeHostFactory {
	if inner == nil || observer == nil {
		return inner
	}
	observed := observedModelRuntimeHostFactory{inner: inner, observer: observer, options: options}
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
	options  ModelObservationOptions
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
		opts.LLM = &observedRuntimeModelLLM{inner: opts.LLM, observer: f.observer, options: f.options}
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
	options  ModelObservationOptions
}

func (llm *observedRuntimeModelLLM) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	if llm == nil || llm.inner == nil {
		return nil, errors.New("workflow model observer has no underlying LLM")
	}
	call := newModelCallWithOptions(ctx, request.Model, llm.observer, llm.options)
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
	call := newModelCallWithOptions(ctx, request.Model, llm.observer, llm.options)
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

	mu            sync.Mutex
	emitted       map[ModelCallPhase]struct{}
	terminal      bool
	options       ModelObservationOptions
	phases        map[ModelCallPhase]int64
	delivery      sync.Mutex
	slow          bool
	slowTimer     *time.Timer
	gatewayWaitMS int64
}

func newModelCall(_ context.Context, model string, observer ModelCallObserver) *runtimeModelCall {
	return &runtimeModelCall{
		callID: uuid.NewString(), model: model, started: time.Now(), observer: observer,
		emitted: make(map[ModelCallPhase]struct{}, 6),
	}
}

func newModelCallWithOptions(ctx context.Context, model string, observer ModelCallObserver, options ModelObservationOptions) *runtimeModelCall {
	call := newModelCall(ctx, model, observer)
	call.options = options
	call.phases = make(map[ModelCallPhase]int64)
	return call
}

func (call *runtimeModelCall) withHTTPTrace(ctx context.Context) context.Context {
	ctx = llmrouter.WithGatewayWaitObserver(ctx, func(elapsed time.Duration) {
		call.mu.Lock()
		defer call.mu.Unlock()
		call.gatewayWaitMS += elapsed.Milliseconds()
	})
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
	call.delivery.Lock()
	defer call.delivery.Unlock()
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
		if call.slowTimer != nil {
			call.slowTimer.Stop()
		}
	}
	event := ModelCallEvent{
		GatewayWaitMS: call.gatewayWaitMS,
		CallID:        call.callID, Model: call.model, Phase: phase,
		ElapsedMS: time.Since(call.started).Milliseconds(), ErrorClass: errorClass,
	}
	if call.options.CompactPhases {
		if phase == ModelCallStarted {
			if call.options.SlowAfter > 0 {
				call.slowTimer = time.AfterFunc(call.options.SlowAfter, func() { call.emitSlowPhases(ctx) })
			}
		} else if !call.terminal {
			call.phases[phase] = event.ElapsedMS
			if !call.slow {
				call.mu.Unlock()
				return
			}
		}
		if call.terminal {
			event.Phases = make(map[ModelCallPhase]int64, len(call.phases))
			for phase, elapsed := range call.phases {
				event.Phases[phase] = elapsed
			}
		}
	}
	call.mu.Unlock()
	defer func() { _ = recover() }()
	call.observer(ctx, event)
}

func (call *runtimeModelCall) emitSlowPhases(ctx context.Context) {
	call.delivery.Lock()
	defer call.delivery.Unlock()
	call.mu.Lock()
	if call.terminal {
		call.mu.Unlock()
		return
	}
	call.slow = true
	var events []ModelCallEvent
	for _, phase := range []ModelCallPhase{ModelCallRequestWritten, ModelCallFirstResponseByte, ModelCallFirstChunk} {
		if elapsed, ok := call.phases[phase]; ok {
			events = append(events, ModelCallEvent{CallID: call.callID, Model: call.model, Phase: phase, ElapsedMS: elapsed})
		}
	}
	call.mu.Unlock()
	for _, event := range events {
		func() { defer func() { _ = recover() }(); call.observer(ctx, event) }()
	}
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
