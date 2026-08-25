package llmrouter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
)

// FallbackLLM wraps a base LLM with retry and model-fallback logic.
// On transient errors (5xx, timeout), it retries the current model with
// exponential backoff. On non-retryable errors (429, 4xx), it switches
// to the next fallback model. When all models are exhausted, returns
// the last error.
type FallbackLLM struct {
	inner           contract.LLM
	primaryModel    string
	fallbackModels  []string
	retriesPerModel int
}

// NewFallbackLLM creates a fallback-aware LLM wrapper.
func NewFallbackLLM(inner contract.LLM, primaryModel string, fallbackModels []string, retriesPerModel int) *FallbackLLM {
	if retriesPerModel <= 0 {
		retriesPerModel = 2
	}
	return &FallbackLLM{
		inner:           inner,
		primaryModel:    primaryModel,
		fallbackModels:  fallbackModels,
		retriesPerModel: retriesPerModel,
	}
}

// Chat tries the primary model, then fallbacks on failure.
func (f *FallbackLLM) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	models := append([]string{f.primaryModel}, f.fallbackModels...)
	var lastErr error

	for _, model := range models {
		req.Model = model
		for attempt := 0; attempt <= f.retriesPerModel; attempt++ {
			resp, err := f.inner.Chat(ctx, req)
			if err == nil {
				return resp, nil
			}
			lastErr = err

			if ctx.Err() != nil {
				return nil, fmt.Errorf("fallback: context cancelled: %w", lastErr)
			}

			if !isRetryable(err) {
				slog.Warn("fallback: non-retryable error, switching model",
					"model", model, "error", err)
				break // try next model
			}

			if attempt < f.retriesPerModel {
				delay := backoff(attempt)
				slog.Info("fallback: retrying after transient error",
					"model", model, "attempt", attempt+1, "delay", delay, "error", err)
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("fallback: context cancelled during retry: %w", lastErr)
				case <-time.After(delay):
				}
			}
		}
	}

	return nil, fmt.Errorf("fallback: all models exhausted: %w", lastErr)
}

// Stream tries the primary model, then fallbacks on failure.
func (f *FallbackLLM) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	models := append([]string{f.primaryModel}, f.fallbackModels...)
	var lastErr error

	for _, model := range models {
		req.Model = model
		for attempt := 0; attempt <= f.retriesPerModel; attempt++ {
			ch, err := f.inner.Stream(ctx, req)
			if err == nil {
				return ch, nil
			}
			lastErr = err

			if ctx.Err() != nil {
				return nil, fmt.Errorf("fallback: context cancelled: %w", lastErr)
			}

			if !isRetryable(err) {
				slog.Warn("fallback: non-retryable error, switching model",
					"model", model, "error", err)
				break
			}

			if attempt < f.retriesPerModel {
				delay := backoff(attempt)
				select {
				case <-ctx.Done():
					return nil, fmt.Errorf("fallback: context cancelled during retry: %w", lastErr)
				case <-time.After(delay):
				}
			}
		}
	}

	return nil, fmt.Errorf("fallback: all models exhausted: %w", lastErr)
}

// httpStatusRe matches "HTTP NNN" in error messages from providers.
var httpStatusRe = regexp.MustCompile(`HTTP (\d{3})`)

// isRetryable classifies an error as transient (worth retrying) or not.
// 5xx and timeout errors are retryable; 4xx (including 429) are not.
func isRetryable(err error) bool {
	msg := err.Error()

	// Empty model responses (HTTP 200 with no content/tool calls, or an empty
	// streamed [DONE]) are treated like 5xx: transient provider-side failures
	// worth retrying on the current model before switching.
	// 中文：空响应是模型侧失败而非调用方配置错误——重试当前模型（受 retries 上限
	// 约束），耗尽后走既有 fallback 链。
	if errors.Is(err, openai.ErrEmptyResponse) {
		return true
	}
	// Providers can close a successful HTTP response before its JSON/SSE body
	// is complete. Treat that transport truncation like a connection reset.
	if errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(msg, "unexpected EOF") {
		return true
	}

	// Check for timeout/context errors.
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return true
	}

	// Parse HTTP status code from provider error messages.
	if m := httpStatusRe.FindStringSubmatch(msg); len(m) == 2 {
		code, _ := strconv.Atoi(m[1])
		return code >= 500
	}

	// Connection errors are retryable.
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection reset") {
		return true
	}

	return false
}

// backoff returns a delay with exponential backoff and ±10% jitter.
// Base: 200ms, max: 2s.
func backoff(attempt int) time.Duration {
	base := 200 * time.Millisecond
	maxDelay := 2 * time.Second

	delay := base
	for i := 0; i < attempt; i++ {
		delay *= 2
	}
	if delay > maxDelay {
		delay = maxDelay
	}

	// Add ±10% jitter.
	jitter := float64(delay) * (0.9 + rand.Float64()*0.2)
	return time.Duration(jitter)
}

// Compile-time check.
var _ contract.LLM = (*FallbackLLM)(nil)
