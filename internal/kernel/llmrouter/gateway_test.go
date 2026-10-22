package llmrouter

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/loom/provider/openai"
	"github.com/jinyitao123/weave/internal/base/execution"
)

type gatewayTestModel struct {
	chat   func(context.Context, contract.ChatRequest) (*contract.ChatResponse, error)
	stream func(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error)
}

func (m gatewayTestModel) Chat(ctx context.Context, r contract.ChatRequest) (*contract.ChatResponse, error) {
	return m.chat(ctx, r)
}
func (m gatewayTestModel) Stream(ctx context.Context, r contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	return m.stream(ctx, r)
}
func gatewayFixture(t *testing.T, p GatewayPolicy, inner contract.LLM) (*ModelGateway, contract.LLM, *modelBucket) {
	t.Helper()
	p.ProviderID = "p"
	p.Model = "m"
	g, err := NewModelGateway(GatewayConfig{Limits: []GatewayPolicy{p}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	config := ProviderConfig{ID: "p", BaseURL: "http://models.invalid", Models: []string{"m"}, AttemptTimeoutSeconds: 1}
	return g, g.WrapProvider(config, inner), g.bucket(config, "m")
}

func TestGatewayConcurrencyAndQueuedCancellation(t *testing.T) {
	var inFlight, peak, calls atomic.Int32
	model := gatewayTestModel{chat: func(ctx context.Context, _ contract.ChatRequest) (*contract.ChatResponse, error) {
		calls.Add(1)
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for old := peak.Load(); current > old && !peak.CompareAndSwap(old, current); old = peak.Load() {
		}
		select {
		case <-time.After(10 * time.Millisecond):
			return &contract.ChatResponse{Content: "ok", Usage: contract.Usage{InputTokens: 1}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	_, llm, _ := gatewayFixture(t, GatewayPolicy{MaxInFlight: 4, Share: 0.5}, model)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := llm.Chat(t.Context(), contract.ChatRequest{Model: "m"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 30 || peak.Load() > 2 || peak.Load() < 2 {
		t.Fatalf("calls=%d concurrency=%d", calls.Load(), peak.Load())
	}
	_, llm, bucket := gatewayFixture(t, GatewayPolicy{MaxInFlight: 1}, model)
	release, err := bucket.acquire(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := llm.Chat(ctx, contract.ChatRequest{Model: "m"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation: %v", err)
	}
	if calls.Load() != before {
		t.Fatal("cancelled queued request reached the provider")
	}
	release(nil, nil)
}

func TestGateway429RetryAfterAndNoReplayAfterStreamStarts(t *testing.T) {
	var attempts atomic.Int32
	model := gatewayTestModel{chat: func(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
		if attempts.Add(1) == 1 {
			return nil, &openai.HTTPError{StatusCode: 429, RetryAfter: "0.02"}
		}
		return &contract.ChatResponse{Content: "ok"}, nil
	}}
	_, llm, bucket := gatewayFixture(t, GatewayPolicy{MaxInFlight: 4}, model)
	start := time.Now()
	if _, err := llm.Chat(t.Context(), contract.ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || time.Since(start) < 20*time.Millisecond {
		t.Fatal("429 did not honor Retry-After")
	}
	bucket.mu.Lock()
	limit := bucket.limit
	bucket.mu.Unlock()
	if limit != 2 {
		t.Fatalf("adaptive limit=%d", limit)
	}
	no := false
	attempts.Store(0)
	_, llm, _ = gatewayFixture(t, GatewayPolicy{MaxInFlight: 4, Retry429: &no}, model)
	if _, err := llm.Chat(t.Context(), contract.ChatRequest{Model: "m"}); err == nil || attempts.Load() != 1 {
		t.Fatal("disabled retry replayed the request")
	}
	attempts.Store(0)
	model.stream = func(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
		attempts.Add(1)
		stream := make(chan contract.StreamChunk, 2)
		stream <- contract.StreamChunk{Content: "partial"}
		stream <- contract.StreamChunk{Err: &openai.HTTPError{StatusCode: 429}}
		close(stream)
		return stream, nil
	}
	_, llm, _ = gatewayFixture(t, GatewayPolicy{MaxInFlight: 2}, model)
	stream, err := llm.Stream(t.Context(), contract.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	if attempts.Load() != 1 {
		t.Fatal("a partially delivered stream was replayed")
	}
}

func TestGatewayPriorityFairnessAndRateBudgets(t *testing.T) {
	_, _, bucket := gatewayFixture(t, GatewayPolicy{MaxInFlight: 1}, gatewayTestModel{})
	hold, err := bucket.acquire(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan string, 4)
	queue := func(key string, priority int) {
		ctx := execution.WithScheduling(t.Context(), execution.SchedulingHints{Priority: priority, FairnessKey: key})
		go func() {
			release, err := bucket.acquire(ctx, 1)
			if err != nil {
				t.Error(err)
				return
			}
			order <- key
			release(nil, nil)
		}()
	}
	for _, item := range []struct {
		key string
		p   int
	}{{"a", 0}, {"a", 0}, {"b", 0}, {"interactive", 1}} {
		queue(item.key, item.p)
	}
	deadline := time.Now().Add(time.Second)
	for {
		bucket.mu.Lock()
		n := len(bucket.waiters)
		bucket.mu.Unlock()
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiters did not arrive")
		}
		time.Sleep(time.Millisecond)
	}
	hold(nil, nil)
	if first := <-order; first != "interactive" {
		t.Fatalf("priority was ignored: %s", first)
	}
	second, third := <-order, <-order
	if second == "a" && third == "a" {
		t.Fatal("one fairness key occupied successive turns while another was waiting")
	}
	<-order
	for _, policy := range []GatewayPolicy{{MaxInFlight: 2, RPM: 1}, {MaxInFlight: 2, TPM: 10}} {
		_, _, limited := gatewayFixture(t, policy, gatewayTestModel{})
		release, err := limited.acquire(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		release(nil, nil)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		if _, err := limited.acquire(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("rate budget bypassed: %v", err)
		}
		cancel()
	}
}

func TestParseRetryAfterIsBounded(t *testing.T) {
	cases := map[string]time.Duration{
		"3":     3 * time.Second,
		"0":     0,
		"86400": gatewayMaxRetryAfter,
		"1e9":   gatewayMaxRetryAfter,
		"busy":  500 * time.Millisecond,
	}
	for value, want := range cases {
		if got := parseRetryAfter(value); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", value, got, want)
		}
	}
	if got := parseRetryAfter(time.Now().Add(48 * time.Hour).UTC().Format(http.TimeFormat)); got != gatewayMaxRetryAfter {
		t.Errorf("a far HTTP date pauses %v, want at most %v", got, gatewayMaxRetryAfter)
	}
}
