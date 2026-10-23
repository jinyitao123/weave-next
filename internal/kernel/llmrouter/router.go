package llmrouter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// DefaultAttemptTimeoutSeconds bounds one provider request, including a
// streamed response body. Meta-team planning and semantic evaluation can
// legitimately process a large frozen evidence package, so the application
// default must cover that workload while remaining finite.
const DefaultAttemptTimeoutSeconds = 900

// ProviderConfig describes a configured LLM provider.
type ProviderConfig struct {
	GatewayProviderID   string                 `json:"-"`
	Transport           http.RoundTripper      `json:"-"`
	CredentialScope     frozen.CredentialScope `json:"credential_scope"`
	CredentialUserID    string                 `json:"credential_user_id,omitempty"`
	CredentialServiceID string                 `json:"credential_service_id,omitempty"`
	ID                  string                 `json:"id"`
	Name                string                 `json:"name"`
	BaseURL             string                 `json:"base_url"`
	// Protocol is the provider's wire protocol: "" or "openai" (default,
	// OpenAI-compatible chat completions) or "systemone" (System One
	// decision protocol). See NormalizeProtocol.
	Protocol string `json:"protocol,omitempty"`
	// APIKey may be empty: openai providers then send an empty bearer token,
	// systemone providers send no Authorization header.
	APIKey         string   `json:"api_key,omitempty"`
	Models         []string `json:"models"`                     // model IDs served by this provider
	JSONObjectMode bool     `json:"json_object_mode,omitempty"` // use json_object instead of json_schema
	// ThinkingDefaultMode 中文：DeepSeek 方言 thinking 开关的无 tools 默认值
	// （"enabled"/"disabled"）。空串 = 未 opt-in：请求体绝不下发 thinking 键，
	// OpenAI/OneAPI 等兼容端点请求零变化。由调用方按 provider profile 显式声明，
	// 不做 model-name 猜测。
	ThinkingDefaultMode string `json:"thinking_default_mode,omitempty"`
	// ThinkingDisableWithTools 中文：携带 tools 时强制 thinking=disabled
	// （明确的非思考工具 profile；历史回传能力不隐式改变模式）。
	ThinkingDisableWithTools bool `json:"thinking_disable_with_tools,omitempty"`
	// AttemptTimeoutSeconds 中文：单个物理 attempt 的超时秒数；0 = 应用默认 900s。
	// 只约束单次 HTTP 调用（含流式读 body），
	// 多轮 ToolLoop 的每一轮各占一次 attempt，互不累计。
	AttemptTimeoutSeconds int `json:"attempt_timeout_seconds,omitempty"`
	// ChatPath is appended to BaseURL; together they are the exact request
	// address. Empty keeps the address providers had before paths were
	// recorded (see frozen.ResolveProviderEndpoint).
	ChatPath string `json:"chat_path,omitempty"`
	// OutputLimitField selects the Chat Completions field the output limit is
	// sent as: max_completion_tokens, or max_tokens when empty.
	OutputLimitField string `json:"output_limit_field,omitempty"`
}

// Endpoint is the base URL and chat path this provider's requests go to.
func (cfg ProviderConfig) Endpoint() frozen.ProviderEndpoint {
	return frozen.ResolveProviderEndpoint(cfg.Protocol, cfg.BaseURL, cfg.ChatPath)
}

// ChatURL is the address every request of this provider goes to.
func (cfg ProviderConfig) ChatURL() string { return cfg.Endpoint().URL() }

// OpenAISDKBaseURL is the base an OpenAI SDK client, which appends
// /chat/completions itself, needs to reach ChatURL; false when the path does
// not end that way, so no SDK base reaches the same address.
func (cfg ProviderConfig) OpenAISDKBaseURL() (string, bool) {
	const sdkPath = "/chat/completions"
	endpoint := cfg.Endpoint()
	if cfg.Protocol == ProtocolSystemOne || !strings.HasSuffix(endpoint.ChatPath, sdkPath) {
		return "", false
	}
	return endpoint.BaseURL + strings.TrimSuffix(endpoint.ChatPath, sdkPath), true
}

// Router implements contract.LLM by dispatching to model-specific providers.
type Router struct {
	mu        sync.RWMutex
	providers map[string]*ProviderConfig // provider ID → config
	models    map[string]string          // model ID → provider ID
	clients   map[string]contract.LLM    // provider ID → LLM client
	fallback  string                     // default model ID
	gateway   *ModelGateway
}

// New creates a Router with a default fallback model.
func New(fallback string) *Router {
	return &Router{
		providers: make(map[string]*ProviderConfig),
		models:    make(map[string]string),
		clients:   make(map[string]contract.LLM),
		fallback:  fallback,
	}
}

// NewProviderClient builds the client for one provider config: the System One
// client for protocol "systemone", otherwise the OpenAI-compatible client.
// Package-level variable so resolver and API tests can substitute a
// recording stub; production wiring never overrides it.
var NewProviderClient = func(cfg ProviderConfig) contract.LLM {
	if cfg.Protocol == ProtocolSystemOne {
		return newSystemOneClient(cfg)
	}
	defaultModel := ""
	if len(cfg.Models) > 0 {
		defaultModel = cfg.Models[0]
	}
	endpoint := cfg.Endpoint()
	opts := []openai.Option{
		openai.WithBaseURL(endpoint.BaseURL),
		openai.WithChatPath(endpoint.ChatPath),
		openai.WithDefaultModel(defaultModel),
	}
	if cfg.JSONObjectMode {
		opts = append(opts, openai.WithJSONObjectMode())
	}
	if cfg.OutputLimitField == frozen.OutputLimitMaxCompletionTokens {
		opts = append(opts, openai.WithMaxCompletionTokens())
	}
	if cfg.ThinkingDefaultMode != "" {
		opts = append(opts, openai.WithThinkingControl(cfg.ThinkingDefaultMode, cfg.ThinkingDisableWithTools))
		opts = append(opts, openai.WithReasoningContentReplay())
	}
	attemptTimeoutSeconds := cfg.AttemptTimeoutSeconds
	if attemptTimeoutSeconds <= 0 {
		attemptTimeoutSeconds = DefaultAttemptTimeoutSeconds
	}
	opts = append(opts, openai.WithAttemptTimeout(time.Duration(attemptTimeoutSeconds)*time.Second))
	if cfg.Transport != nil {
		opts = append(opts, openai.WithHTTPTransport(cfg.Transport))
	}
	client := openai.New(cfg.APIKey, opts...)
	if cfg.JSONObjectMode {
		return &locallyVerifiedJSONObjectLLM{inner: client}
	}
	return client
}

// RegisterProvider adds or updates a provider and its models.
func (r *Router) RegisterProvider(cfg ProviderConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := cfg
	stored.Models = append([]string(nil), cfg.Models...)

	// Remove old model mappings if provider existed.
	if old, ok := r.providers[stored.ID]; ok {
		for _, m := range old.Models {
			delete(r.models, m)
		}
	}

	// Store config.
	r.providers[stored.ID] = &stored

	// Create OpenAI-compatible client.
	if r.gateway != nil {
		stored.Transport = r.gateway.transport(stored.BaseURL)
	}
	client := NewProviderClient(stored)
	if r.gateway != nil {
		client = r.gateway.WrapProvider(stored, client)
	}
	r.clients[stored.ID] = client

	// Map each model to this provider.
	for _, m := range stored.Models {
		r.models[m] = stored.ID
	}
}

// SetGateway configures deployment capacity policy for subsequently registered
// providers. It does not change any provider's immutable execution definition.
func (r *Router) SetGateway(gateway *ModelGateway) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gateway = gateway
}

// RemoveProvider removes a provider and its model mappings.
func (r *Router) RemoveProvider(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if cfg, ok := r.providers[id]; ok {
		for _, m := range cfg.Models {
			delete(r.models, m)
		}
		delete(r.providers, id)
		delete(r.clients, id)
	}
}

// ListProviders returns all configured providers (keys masked).
func (r *Router) ListProviders() []ProviderConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []ProviderConfig
	for _, cfg := range r.providers {
		masked := *cfg
		masked.APIKey = maskKey(cfg.APIKey)
		out = append(out, masked)
	}
	return out
}

// SystemProvider returns one exact process-configured provider. The returned
// value owns its model slice, so mirror callers cannot mutate router state.
// API keys are deliberately preserved: the explicit mirror path encrypts the
// source key directly into a workspace credential slot.
func (r *Router) SystemProvider(id string) (ProviderConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cfg, ok := r.providers[id]
	if !ok || cfg == nil {
		return ProviderConfig{}, false
	}
	copied := *cfg
	copied.Models = append([]string(nil), cfg.Models...)
	return copied, true
}

// Register adds a single model → provider mapping (test/system-stub seam).
func (r *Router) Register(model string, provider contract.LLM) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Use model as provider ID for legacy registrations.
	r.clients[model] = provider
	r.models[model] = model
}

func (r *Router) resolve(model string) (contract.LLM, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if model == "" {
		model = r.fallback
	}

	// Check model → provider mapping.
	if providerID, ok := r.models[model]; ok {
		if client, ok := r.clients[providerID]; ok {
			return client, nil
		}
	}

	return nil, fmt.Errorf("llmrouter: model %q not registered — configure a provider in Settings", model)
}

// Chat dispatches to the correct provider based on req.Model.
func (r *Router) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	if req.Model == "" {
		req.Model = r.fallback
	}
	provider, err := r.resolve(req.Model)
	if err != nil {
		return nil, err
	}
	return provider.Chat(ctx, req)
}

// Stream dispatches to the correct provider based on req.Model.
func (r *Router) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	if req.Model == "" {
		req.Model = r.fallback
	}
	provider, err := r.resolve(req.Model)
	if err != nil {
		return nil, err
	}
	return provider.Stream(ctx, req)
}

func maskKey(key string) string {
	if key == "" {
		return "" // unset, not masked
	}
	if len(key) <= 8 {
		return "********"
	}
	return key[:8] + "********"
}

// Compile-time interface check.
var _ contract.LLM = (*Router)(nil)

// ObserveModelProtocolCall keeps provider coupling inside the adapter. It
// attaches a passive observer, sanitizes its metadata, and separately measures
// the returned standard ToolCalls. Request/response/error are never rewritten.
func ObserveModelProtocolCall(ctx context.Context, inner contract.LLM, request contract.ChatRequest,
	observe func(ModelProtocolObservation), normalized func(NormalizedModelProtocol)) (*contract.ChatResponse, error) {
	if observe != nil {
		ctx = openai.WithProtocolObserver(ctx, openai.ProtocolObserverOptions{MaxDataFrames: 4096, MaxToolDeltas: 4096, MaxToolIndexes: protocolToolLimit}, func(value contract.ProtocolObservation) {
			observe(boundedProtocolObservation(value))
		})
	}
	response, err := inner.Chat(ctx, request)
	if normalized != nil {
		normalized(normalizedModelProtocol(response, err))
	}
	return response, err
}

// Call failure classes that only typed evidence can establish: an HTTP status
// or a provider adapter's own error type. Nothing is read from error text.
const (
	CallFailureRateLimited        = "rate_limited"
	CallFailureAuth               = "auth"
	CallFailureUnsupportedRequest = "unsupported_request"
	CallFailureUpstream5xx        = "upstream_5xx"
	CallFailureStreamTruncated    = "stream_truncated"
	CallFailureEmptyResponse      = "provider_empty_response"
	CallFailureOutputLimit        = "provider_output_limit"
	// CallFailureStageTimeout is a call ended by its member's wait budget;
	// whether repeating it could succeed is the caller's decision.
	CallFailureStageTimeout = "stage_timeout"
)

// CallFailure is the safe, typed account of a failed model call. Class is
// empty when the error carries no typed evidence. HTTPStatus is zero unless
// the provider answered with one, and FinishReason is one of the known
// OpenAI-compatible values or empty. Retryable is nil when the evidence does
// not say whether repeating the same request could succeed.
type CallFailure struct {
	Class        string
	HTTPStatus   int
	FinishReason string
	Retryable    *bool
	// Stage names the wait budget stage that ran out: first_chunk or
	// stream_idle.
	Stage string
}

// DiagnoseCallError classifies a model call error from typed evidence only:
// a status reported through HTTPStatus() (OpenAI-compatible and System One
// providers) or the OpenAI adapter's empty-response and stream-close errors.
func DiagnoseCallError(err error) CallFailure {
	if err == nil {
		return CallFailure{}
	}
	var stage *openai.StageTimeoutError
	if errors.As(err, &stage) {
		return CallFailure{Class: CallFailureStageTimeout, Stage: stage.Stage}
	}
	retry := func(value bool) *bool { return &value }
	var status interface{ HTTPStatus() int }
	if errors.As(err, &status) {
		code := status.HTTPStatus()
		failure := CallFailure{HTTPStatus: code}
		switch {
		case code == http.StatusTooManyRequests:
			failure.Class, failure.Retryable = CallFailureRateLimited, retry(true)
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			failure.Class, failure.Retryable = CallFailureAuth, retry(false)
		case code >= 500:
			failure.Class, failure.Retryable = CallFailureUpstream5xx, retry(true)
		case code >= 400:
			failure.Class, failure.Retryable = CallFailureUnsupportedRequest, retry(false)
		}
		return failure
	}
	var empty *openai.EmptyResponseError
	if errors.As(err, &empty) {
		failure := CallFailure{FinishReason: knownFinishReason(empty.FinishReason)}
		switch {
		case empty.FinishReason == "length":
			// The output budget ran out; the same request runs out again.
			failure.Class, failure.Retryable = CallFailureOutputLimit, retry(false)
		case empty.Stream && (empty.CloseKind == "eof" || empty.CloseKind == "scanner"):
			failure.Class, failure.Retryable = CallFailureStreamTruncated, retry(true)
		default:
			failure.Class = CallFailureEmptyResponse
		}
		return failure
	}
	if errors.Is(err, openai.ErrStreamClosedWithoutDone) || errors.Is(err, openai.ErrStreamScanner) {
		return CallFailure{Class: CallFailureStreamTruncated, Retryable: retry(true)}
	}
	if errors.Is(err, openai.ErrJSONModeInstructionMissing) || errors.Is(err, openai.ErrToolChoiceInvalid) ||
		errors.Is(err, openai.ErrToolChoiceUnsupported) {
		// Refused before sending: the request itself is not acceptable.
		return CallFailure{Class: CallFailureUnsupportedRequest, Retryable: retry(false)}
	}
	return CallFailure{}
}

// knownFinishReason passes through only the documented finish_reason values,
// so a provider cannot place arbitrary text in a diagnostic.
func knownFinishReason(reason string) string {
	switch reason {
	case "stop", "length", "tool_calls", "content_filter", "function_call":
		return reason
	}
	return ""
}
