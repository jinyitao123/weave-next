package llmrouter

import (
	"context"
	"fmt"
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
	// （DeepSeek 官方 OMP 适配要求；M1 fail-safe，不做 reasoning 连续性回传）。
	ThinkingDisableWithTools bool `json:"thinking_disable_with_tools,omitempty"`
	// AttemptTimeoutSeconds 中文：单个物理 attempt 的超时秒数；0 = 应用默认 900s。
	// 只约束单次 HTTP 调用（含流式读 body），
	// 多轮 ToolLoop 的每一轮各占一次 attempt，互不累计。
	AttemptTimeoutSeconds int `json:"attempt_timeout_seconds,omitempty"`
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

// NewProviderClient builds the OpenAI-compatible client for one provider
// config. Package-level variable so resolver and API tests can substitute a
// recording stub; production wiring never overrides it.
var NewProviderClient = func(cfg ProviderConfig) contract.LLM {
	defaultModel := ""
	if len(cfg.Models) > 0 {
		defaultModel = cfg.Models[0]
	}
	opts := []openai.Option{
		openai.WithBaseURL(cfg.BaseURL),
		openai.WithDefaultModel(defaultModel),
	}
	if cfg.JSONObjectMode {
		opts = append(opts, openai.WithJSONObjectMode())
	}
	if cfg.ThinkingDefaultMode != "" {
		opts = append(opts, openai.WithThinkingControl(cfg.ThinkingDefaultMode, cfg.ThinkingDisableWithTools))
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
