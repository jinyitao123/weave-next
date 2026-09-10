package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/base/frozen"
	"github.com/jinyitao123/weave/internal/kernel/compiler"
	"github.com/jinyitao123/weave/internal/kernel/credentials"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
	"github.com/jinyitao123/weave/internal/kernel/mcphost"
)

const CodeRuntimeHostUnsupported = "workflow_runtime_host_unsupported"

var ErrRuntimeHostUnsupported = errors.New("workflow runtime host unsupported")

type RuntimeHostFactoryFunc func(
	context.Context,
	frozen.FrozenExecutionBundle,
	RuntimeCredentialResolver,
) (compiler.FrozenBuildOpts, io.Closer, error)

func (f RuntimeHostFactoryFunc) Build(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	resolver RuntimeCredentialResolver,
) (compiler.FrozenBuildOpts, io.Closer, error) {
	return f(ctx, bundle, resolver)
}

func NewRuntimeHostFactory() RuntimeHostFactory {
	return RuntimeHostFactoryFunc(buildRuntimeHosts)
}

// RuntimeToolEvent is the secret-free observable envelope for one frozen
// workflow tool call. Arguments and result content deliberately stay out of
// this contract; callers can correlate real tool use without retaining
// credentials or model-private working state.
type RuntimeToolEvent struct {
	Kind        string
	Tool        string
	CallID      string
	ResultError bool
}

type RuntimeToolObserver func(context.Context, RuntimeToolEvent)

// ObserveRuntimeTools decorates a runtime host factory with tool lifecycle
// observation while preserving every existing frozen hook.
func ObserveRuntimeTools(inner RuntimeHostFactory, observer RuntimeToolObserver) RuntimeHostFactory {
	if inner == nil || observer == nil {
		return inner
	}
	return RuntimeHostFactoryFunc(func(ctx context.Context, bundle frozen.FrozenExecutionBundle, resolver RuntimeCredentialResolver) (compiler.FrozenBuildOpts, io.Closer, error) {
		opts, closer, err := inner.Build(ctx, bundle, resolver)
		if err != nil {
			return opts, closer, err
		}
		opts.Hooks.ToolHooks = append(opts.Hooks.ToolHooks, contract.ToolHook{
			Pre: func(ctx context.Context, call contract.ToolCall) (contract.ToolCall, error) {
				observer(ctx, RuntimeToolEvent{Kind: "tool_started", Tool: call.Name, CallID: call.ID})
				return call, nil
			},
			Post: func(ctx context.Context, call contract.ToolCall, result *contract.ToolResult) error {
				observer(ctx, RuntimeToolEvent{Kind: "tool_completed", Tool: call.Name, CallID: call.ID,
					ResultError: result != nil && result.IsError})
				return nil
			},
		})
		return opts, closer, nil
	})
}

func buildRuntimeHosts(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	resolver RuntimeCredentialResolver,
) (compiler.FrozenBuildOpts, io.Closer, error) {
	return buildRuntimeHostsWithTransportFactory(
		ctx,
		bundle,
		resolver,
		newRuntimeMCPTransport,
	)
}

type runtimeMCPTransport interface {
	http.RoundTripper
	CloseIdleConnections()
}

func buildRuntimeHostsWithTransportFactory(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	resolver RuntimeCredentialResolver,
	newTransport func() runtimeMCPTransport,
) (compiler.FrozenBuildOpts, io.Closer, error) {
	return buildRuntimeHostsWithLLM(
		ctx,
		bundle,
		resolver,
		newTransport,
		nil,
	)
}

func buildRuntimeHostsWithLLM(
	ctx context.Context,
	bundle frozen.FrozenExecutionBundle,
	resolver RuntimeCredentialResolver,
	newTransport func() runtimeMCPTransport,
	llmOverride contract.LLM,
) (compiler.FrozenBuildOpts, io.Closer, error) {
	if err := compiler.ValidateStandardMCPBindings(bundle); err != nil {
		return compiler.FrozenBuildOpts{}, nil, err
	}
	for _, binding := range bundle.MCPBindings {
		if binding.Transport != "http" {
			detail := "MCP transport is not supported"
			if binding.Transport == "stdio" {
				detail = "stdio MCP transport is not supported"
			}
			return compiler.FrozenBuildOpts{}, nil, runtimeHostUnsupportedError(detail)
		}
	}
	if bundle.Runtime != nil && bundle.Runtime.Engine != "loom" {
		return compiler.FrozenBuildOpts{}, nil, runtimeHostUnsupportedError("stdio runtime host assembly is not supported")
	}
	if bundle.Runtime != nil && bundle.Runtime.Engine == "loom" && bundle.Runtime.RuntimeID != "" {
		return compiler.FrozenBuildOpts{}, nil, runtimeHostUnsupportedError("loom runtime binding with runtime ID is not supported")
	}

	bindings := make([]frozen.FrozenModelBinding, 0, 1+len(bundle.FallbackModels))
	if bundle.Agent.Model != "" {
		bindings = append(bindings, bundle.PrimaryModel)
	}
	bindings = append(bindings, bundle.FallbackModels...)
	if len(bindings) == 0 && llmOverride == nil {
		return compiler.FrozenBuildOpts{}, nil, runtimeHostUnsupportedError("runtime host assembly requires at least one model binding")
	}

	llm := llmOverride
	if llm == nil {
		primaryModelID := bindings[0].ModelID
		router := llmrouter.New(primaryModelID)
		fallbackModelIDs := make([]string, 0, len(bindings)-1)
		for index, binding := range bindings {
			material, err := resolver.Resolve(ctx, credentials.ResolveRequest{Reference: binding.CredentialRef})
			if err != nil {
				return compiler.FrozenBuildOpts{}, nil, err
			}
			router.RegisterProvider(llmrouter.ProviderConfig{
				ID:      fmt.Sprintf("%d#%s#%s", index, binding.ProviderID, binding.ModelID),
				BaseURL: binding.BaseURL, APIKey: string(material.Value()),
				Models: []string{binding.ModelID}, JSONObjectMode: binding.JSONObjectMode,
			})
			if index != 0 {
				fallbackModelIDs = append(fallbackModelIDs, binding.ModelID)
			}
		}
		llm = compiler.FrozenBuildOpts{LLM: router}.LLM
		if len(fallbackModelIDs) != 0 {
			llm = llmrouter.NewFallbackLLM(router, primaryModelID, fallbackModelIDs, int(bundle.Agent.Fallback.Retries))
		}
	}
	llm = &runtimeStreamingLLM{inner: llm}

	var tools contract.ToolDispatcher = &compiler.NoOpDispatcher{}
	closer := &runtimeHostCloser{}
	mcpDispatchers := make([]contract.ToolDispatcher, 0, len(bundle.MCPBindings))
	for index, binding := range bundle.MCPBindings {
		material, err := resolver.Resolve(ctx, credentials.ResolveRequest{Reference: binding.AccessRef})
		if err != nil {
			_ = closer.Close()
			return compiler.FrozenBuildOpts{}, nil, err
		}
		headers := runtimeMCPHeaderStrings(material)
		transport := newTransport()
		client := &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		host := mcphost.NewHTTPHost(
			binding.URL,
			mcphost.WithHeaders(headers),
			mcphost.WithFilter(binding.Filter),
			mcphost.WithHTTPClient(client),
		)
		clearRuntimeMCPHeaderStrings(headers)
		var checkedHost contract.ToolDispatcher = host
		if len(binding.Tools) != 0 {
			frozenHost := mcphost.NewFrozenMCPDispatcher(host, binding)
			bound, err := frozenHost.BoundContract()
			if err != nil {
				transport.CloseIdleConnections()
				_ = closer.Close()
				return compiler.FrozenBuildOpts{}, nil, err
			}
			mcphost.WithToolContract(bound)(host)
			checkedHost = frozenHost
		}
		writeGate := mcphost.NewWriteGateDispatcherForServer(
			checkedHost,
			binding.WorkspaceID, bundle.Agent.Name, binding.ServerID, binding.URL, "",
			binding.WriteTools,
		)
		dispatcher := &runtimeStrictMCPDispatcher{inner: writeGate, bindingIndex: index}
		mcpDispatchers = append(mcpDispatchers, dispatcher)
		closer.closers = append(closer.closers, runtimeHTTPTransportCloser{transport: transport})
	}
	if len(mcpDispatchers) == 1 {
		tools = mcpDispatchers[0]
	} else if len(mcpDispatchers) > 1 {
		tools = mcphost.NewCompositeDispatcher(mcpDispatchers...)
	}

	return compiler.FrozenBuildOpts{
		LLM: llm, Tools: tools,
		CheckpointStore: loom.NewMemStore(), AuditStore: loom.NewMemStore(),
	}, closer, nil
}

type runtimeStreamingLLM struct {
	inner contract.LLM
}

func (l *runtimeStreamingLLM) Chat(
	ctx context.Context,
	request contract.ChatRequest,
) (*contract.ChatResponse, error) {
	if l == nil || l.inner == nil {
		return nil, errors.New("workflow streaming LLM is unavailable")
	}
	stream, err := l.inner.Stream(ctx, request)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, errors.New("workflow streaming LLM returned no stream")
	}
	var content strings.Builder
	var toolCalls []contract.ToolCall
	var usage contract.Usage
	done := false
	for chunk := range stream {
		content.WriteString(chunk.Content)
		toolCalls = append(toolCalls, chunk.ToolCalls...)
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if chunk.Done {
			done = true
		}
	}
	if !done && content.Len() == 0 && len(toolCalls) == 0 && usage == (contract.Usage{}) {
		response, chatErr := l.inner.Chat(ctx, request)
		if chatErr != nil {
			return nil, fmt.Errorf("workflow model stream ended before done and chat fallback failed: %w", chatErr)
		}
		if response == nil ||
			(response.Content == "" && len(response.ToolCalls) == 0 && response.Usage == (contract.Usage{})) {
			return nil, errors.New("workflow model stream ended before done")
		}
		return response, nil
	}
	return &contract.ChatResponse{
		Content:   content.String(),
		ToolCalls: toolCalls,
		Usage:     usage,
	}, nil
}

func (l *runtimeStreamingLLM) Stream(
	ctx context.Context,
	request contract.ChatRequest,
) (<-chan contract.StreamChunk, error) {
	if l == nil || l.inner == nil {
		return nil, errors.New("workflow streaming LLM is unavailable")
	}
	return l.inner.Stream(ctx, request)
}

type runtimeStrictMCPDispatcher struct {
	inner        contract.ToolDispatcher
	bindingIndex int
}

func (d *runtimeStrictMCPDispatcher) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	tools, err := d.inner.ListTools(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: MCP host %d list tools: %w",
			mcphost.ErrFailClosed,
			d.bindingIndex,
			err,
		)
	}
	return tools, nil
}

func (d *runtimeStrictMCPDispatcher) Dispatch(
	ctx context.Context,
	call contract.ToolCall,
) (*contract.ToolResult, error) {
	result, err := d.inner.Dispatch(ctx, call)
	if err != nil {
		return nil, fmt.Errorf(
			"%w: MCP host %d dispatch: %w",
			mcphost.ErrFailClosed,
			d.bindingIndex,
			err,
		)
	}
	return result, nil
}

func newRuntimeMCPTransport() runtimeMCPTransport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func runtimeMCPHeaderStrings(material credentials.SecretMaterial) map[string]string {
	byteHeaders := material.Headers()
	headers := make(map[string]string, len(byteHeaders))
	for name, value := range byteHeaders {
		headers[name] = string(value)
		for index := range value {
			value[index] = 0
		}
		delete(byteHeaders, name)
	}
	return headers
}

func clearRuntimeMCPHeaderStrings(headers map[string]string) {
	// This clears only temporary copies. SecretMaterial and HTTPHost-owned
	// header strings remain resident until their respective owners release them.
	for name := range headers {
		headers[name] = ""
		delete(headers, name)
	}
}

type runtimeHTTPTransportCloser struct {
	transport runtimeMCPTransport
}

func (c runtimeHTTPTransportCloser) Close() error {
	c.transport.CloseIdleConnections()
	return nil
}

type runtimeHostFactoryError struct {
	detail string
}

func (e *runtimeHostFactoryError) Error() string { return e.detail }
func (e *runtimeHostFactoryError) Code() string  { return CodeRuntimeHostUnsupported }
func (e *runtimeHostFactoryError) Unwrap() error { return ErrRuntimeHostUnsupported }

func runtimeHostUnsupportedError(detail string) error {
	return &runtimeHostFactoryError{detail: detail}
}

// runtimeHostCloser owns factory-created resources and closes them in reverse
// registration order.
type runtimeHostCloser struct {
	mu      sync.Mutex
	closed  bool
	closers []io.Closer
}

func (c *runtimeHostCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	closeErrors := make([]error, 0, len(c.closers))
	for index := len(c.closers) - 1; index >= 0; index-- {
		closeErrors = append(closeErrors, c.closers[index].Close())
	}
	return errors.Join(closeErrors...)
}
