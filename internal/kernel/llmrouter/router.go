package llmrouter

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/anthropic"
	"github.com/jinyitao123/loom/provider/openai"
	"github.com/jinyitao123/loom/provider/openairesponses"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

// DefaultAttemptTimeoutSeconds bounds one provider request, including a
// streamed response body. Meta-team planning and semantic evaluation can
// legitimately process a large frozen evidence package, so the application
// default must cover that workload while remaining finite.
const DefaultAttemptTimeoutSeconds = 900

// ProviderConfig describes a configured LLM provider.
type ProviderConfig struct {
	Protocol            string                 `json:"protocol"`
	CredentialScope     frozen.CredentialScope `json:"credential_scope"`
	CredentialUserID    string                 `json:"credential_user_id,omitempty"`
	CredentialServiceID string                 `json:"credential_service_id,omitempty"`
	ID                  string                 `json:"id"`
	Name                string                 `json:"name"`
	BaseURL             string                 `json:"base_url"`
	APIKey              string                 `json:"api_key,omitempty"`
	Models              []string               `json:"models"`                     // model IDs served by this provider
	JSONObjectMode      bool                   `json:"json_object_mode,omitempty"` // use json_object instead of json_schema
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
	// address (see frozen.NormalizeProviderEndpoint).
	ChatPath string `json:"chat_path,omitempty"`
}

// NormalizeEndpoint puts BaseURL and ChatPath in stored form, filling the
// protocol's default path when none is given, or explains why they are refused.
func (cfg *ProviderConfig) NormalizeEndpoint() error {
	endpoint, err := frozen.NormalizeProviderEndpoint(cfg.Protocol, cfg.BaseURL, cfg.ChatPath)
	if err != nil {
		return err
	}
	cfg.BaseURL, cfg.ChatPath = endpoint.BaseURL, endpoint.ChatPath
	return nil
}

// ChatURL is the address every request of this provider goes to.
func (cfg ProviderConfig) ChatURL() string {
	return frozen.ProviderEndpoint{BaseURL: cfg.BaseURL, ChatPath: cfg.ChatPath}.URL()
}

// OpenAISDKBaseURL is the base an OpenAI SDK client (which appends
// /chat/completions itself) needs to reach ChatURL; false when the provider's
// path does not end that way, so no SDK base reaches the same address.
func (cfg ProviderConfig) OpenAISDKBaseURL() (string, bool) {
	const sdkPath = "/chat/completions"
	if cfg.Protocol != frozen.ProtocolOpenAICompletions || !strings.HasSuffix(cfg.ChatPath, sdkPath) {
		return "", false
	}
	return cfg.adapterBaseURL(sdkPath), true
}

// adapterBaseURL is what a native adapter, which appends its own fixed path,
// must be given so that its request still goes to ChatURL.
func (cfg ProviderConfig) adapterBaseURL(fixedSuffix string) string {
	return cfg.BaseURL + strings.TrimSuffix(cfg.ChatPath, fixedSuffix)
}

// Router implements contract.LLM by dispatching to model-specific providers.
type Router struct {
	mu        sync.RWMutex
	providers map[string]*ProviderConfig // provider ID → config
	models    map[string]string          // model ID → provider ID
	clients   map[string]contract.LLM    // provider ID → LLM client
	fallback  string                     // default model ID
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

// NewProviderClient builds the explicit native-protocol client for one provider
// config. Package-level variable so resolver and API tests can substitute a
// recording stub; production wiring never overrides it.
var NewProviderClient = func(cfg ProviderConfig) contract.LLM {
	if err := cfg.Validate(); err != nil {
		return &invalidProviderLLM{err: err}
	}
	defaultModel := ""
	if len(cfg.Models) > 0 {
		defaultModel = cfg.Models[0]
	}
	seconds := cfg.AttemptTimeoutSeconds
	if seconds == 0 {
		seconds = DefaultAttemptTimeoutSeconds
	}
	timeout := time.Duration(seconds) * time.Second
	switch cfg.Protocol {
	case frozen.ProtocolAnthropicMessages:
		return anthropic.New(cfg.APIKey, anthropic.WithBaseURL(cfg.adapterBaseURL("/v1/messages")), anthropic.WithDefaultModel(defaultModel), anthropic.WithAttemptTimeout(timeout))
	case frozen.ProtocolOpenAIResponses:
		return openairesponses.New(cfg.APIKey, openairesponses.WithBaseURL(cfg.adapterBaseURL("/responses")), openairesponses.WithDefaultModel(defaultModel), openairesponses.WithAttemptTimeout(timeout))
	}
	opts := []openai.Option{
		openai.WithBaseURL(cfg.BaseURL),
		openai.WithChatPath(cfg.ChatPath),
		openai.WithDefaultModel(defaultModel),
	}
	if cfg.JSONObjectMode {
		opts = append(opts, openai.WithJSONObjectMode())
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
	r.clients[stored.ID] = NewProviderClient(stored)

	// Map each model to this provider.
	for _, m := range stored.Models {
		r.models[m] = stored.ID
	}
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

// Validate rejects incomplete or incompatible profiles before persistence or use.
func (cfg ProviderConfig) Validate() error {
	if err := frozen.ValidateProviderProfile(cfg.Protocol, cfg.JSONObjectMode, cfg.ThinkingDefaultMode, cfg.ThinkingDisableWithTools, cfg.AttemptTimeoutSeconds); err != nil {
		return err
	}
	return frozen.ValidateProviderEndpoint(cfg.Protocol, cfg.BaseURL, cfg.ChatPath)
}

type invalidProviderLLM struct{ err error }

func (l *invalidProviderLLM) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return nil, l.err
}
func (l *invalidProviderLLM) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return nil, l.err
}
