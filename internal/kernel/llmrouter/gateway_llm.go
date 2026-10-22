package llmrouter

import (
	"context"
	"errors"
	"time"

	"github.com/jinyitao123/loom/contract"
)

type gatewayLLM struct {
	inner   contract.LLM
	gateway *ModelGateway
	config  ProviderConfig
}

func (g *ModelGateway) WrapProvider(config ProviderConfig, inner contract.LLM) contract.LLM {
	if g == nil {
		return inner
	}
	return &gatewayLLM{inner: inner, gateway: g, config: config}
}
func (l *gatewayLLM) request(ctx context.Context, request contract.ChatRequest) (context.Context, context.CancelFunc, *modelBucket, int) {
	model := request.Model
	if model == "" && len(l.config.Models) > 0 {
		model = l.config.Models[0]
	}
	bucket := l.gateway.bucket(l.config, model)
	if bucket == nil {
		return ctx, func() {}, nil, 0
	}
	seconds := l.config.AttemptTimeoutSeconds
	if seconds <= 0 {
		seconds = DefaultAttemptTimeoutSeconds
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	tokens := request.MaxTokens
	if tokens <= 0 {
		tokens = 1024
	}
	addTokens := func(extra int) { tokens += min(extra, int(^uint(0)>>1)-tokens) }
	for _, message := range request.Messages {
		addTokens((len(message.Content) + 2) / 3)
	}
	if request.Schema != nil {
		addTokens(len(*request.Schema) / 3)
	}
	for _, tool := range request.Tools {
		addTokens((len(tool.Description) + len(tool.InputSchema)) / 3)
	}
	return ctx, cancel, bucket, tokens
}

func (l *gatewayLLM) Chat(ctx context.Context, request contract.ChatRequest) (*contract.ChatResponse, error) {
	ctx, cancel, bucket, tokens := l.request(ctx, request)
	defer cancel()
	if bucket == nil {
		return l.inner.Chat(ctx, request)
	}
	for attempt := 0; ; attempt++ {
		start := time.Now()
		release, err := bucket.acquire(ctx, tokens)
		observeGatewayWait(ctx, time.Since(start))
		if err != nil {
			return nil, err
		}
		response, err := l.inner.Chat(ctx, request)
		var actual *int
		if response != nil && response.Usage.InputTokens+response.Usage.OutputTokens > 0 {
			count := response.Usage.InputTokens + response.Usage.OutputTokens
			actual = &count
		}
		release(actual, err)
		status, _ := gatewayHTTPFailure(err)
		if status != 429 || !enabled(bucket.policy.Retry429) || attempt >= 3 {
			return response, err
		}
	}
}

func (l *gatewayLLM) Stream(ctx context.Context, request contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	callCtx, cancel, bucket, tokens := l.request(ctx, request)
	if bucket == nil {
		cancel()
		return l.inner.Stream(ctx, request)
	}
	for attempt := 0; ; attempt++ {
		start := time.Now()
		release, err := bucket.acquire(callCtx, tokens)
		observeGatewayWait(callCtx, time.Since(start))
		if err != nil {
			cancel()
			return nil, err
		}
		stream, err := l.inner.Stream(callCtx, request)
		if err != nil {
			// A rejected stream has not started. Drain any producer before a retry.
			if stream != nil {
				for range stream {
				}
			}
			release(nil, err)
			status, _ := gatewayHTTPFailure(err)
			if status == 429 && enabled(bucket.policy.Retry429) && attempt < 3 {
				continue
			}
			cancel()
			return nil, err
		}
		if stream == nil {
			release(nil, errors.New("model returned no stream"))
			cancel()
			return nil, errors.New("model returned no stream")
		}
		out := make(chan contract.StreamChunk, 32)
		go func() {
			defer close(out)
			defer cancel()
			var actual *int
			var streamErr error
			forward := true
			for chunk := range stream {
				if chunk.Usage != nil {
					count := chunk.Usage.InputTokens + chunk.Usage.OutputTokens
					actual = &count
				}
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
				if forward {
					select {
					case out <- chunk:
					case <-callCtx.Done():
						forward = false
						streamErr = callCtx.Err()
						cancel()
					}
				}
			}
			if callCtx.Err() != nil {
				streamErr = callCtx.Err()
			}
			release(actual, streamErr)
		}()
		return out, nil
	}
}

type gatewayWaitKey struct{}

func WithGatewayWaitObserver(ctx context.Context, observer func(time.Duration)) context.Context {
	return context.WithValue(ctx, gatewayWaitKey{}, observer)
}
func observeGatewayWait(ctx context.Context, elapsed time.Duration) {
	if observer, _ := ctx.Value(gatewayWaitKey{}).(func(time.Duration)); observer != nil {
		observer(elapsed)
	}
}
