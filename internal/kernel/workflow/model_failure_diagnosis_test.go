package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/llmrouter"
)

// TestModelCallFailuresAreTypedFromRecordedResponses drives a real
// OpenAI-compatible client against recorded HTTP and SSE responses and checks
// the failed event's class and safe facts. Error and response bodies carry a
// marker that must never reach the event.
func TestModelCallFailuresAreTypedFromRecordedResponses(t *testing.T) {
	const marker = "PRIVATE-BODY-TEXT"
	chunk := func(writer http.ResponseWriter, delta, finish string) {
		finishJSON := "null"
		if finish != "" {
			finishJSON = `"` + finish + `"`
		}
		_, _ = fmt.Fprintf(writer, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":%s}]}\n\n", delta, finishJSON)
		writer.(http.Flusher).Flush()
	}
	stream := func(writer http.ResponseWriter) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
	}
	yes, no := true, false
	cases := []struct {
		name          string
		handler       http.HandlerFunc
		ctx           func() (context.Context, context.CancelFunc)
		phase         ModelCallPhase
		class         ModelCallErrorClass
		status        int
		finish        string
		retryable     *bool
		streamStarted bool
	}{
		{name: "rate limited", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			http.Error(w, marker, http.StatusTooManyRequests)
		}, class: ModelCallErrorRateLimited, status: 429, retryable: &yes},
		{name: "unauthorized", handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, marker, http.StatusUnauthorized) },
			class: ModelCallErrorAuth, status: 401, retryable: &no},
		{name: "unsupported parameter", handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, marker, http.StatusBadRequest) },
			class: ModelCallErrorUnsupportedRequest, status: 400, retryable: &no},
		{name: "bad gateway", handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, marker, http.StatusBadGateway) },
			class: ModelCallErrorUpstream5xx, status: 502, retryable: &yes},
		{name: "end of stream after the first chunk", handler: func(w http.ResponseWriter, _ *http.Request) {
			stream(w)
			chunk(w, `{"role":"assistant","content":"`+marker+`"}`, "")
		}, class: ModelCallErrorStreamTruncated, retryable: &yes, streamStarted: true},
		{name: "empty stop", handler: func(w http.ResponseWriter, _ *http.Request) {
			stream(w)
			chunk(w, `{}`, "stop")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}, class: ModelCallErrorEmptyResponse, finish: "stop"},
		{name: "empty length", handler: func(w http.ResponseWriter, _ *http.Request) {
			stream(w)
			chunk(w, `{}`, "length")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}, class: ModelCallErrorOutputLimit, finish: "length", retryable: &no},
		{name: "caller cancellation", handler: awaitCancellation,
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(100*time.Millisecond, cancel)
				return ctx, cancel
			}, phase: ModelCallCancelled, class: ModelCallErrorContextCancelled},
		{name: "deadline", handler: awaitCancellation,
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			}, class: ModelCallErrorContextDeadline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			event := failedModelEvent(t, server.URL, tc.ctx, tc.phase)
			if event.ErrorClass != tc.class || event.HTTPStatus != tc.status || event.FinishReason != tc.finish ||
				fmt.Sprint(event.Retryable) != fmt.Sprint(tc.retryable) && (event.Retryable == nil) != (tc.retryable == nil) ||
				(event.Retryable != nil && tc.retryable != nil && *event.Retryable != *tc.retryable) || event.StreamStarted != tc.streamStarted {
				t.Fatalf("event = %+v (retryable %v), want class %s status %d finish %q retryable %v stream_started %v",
					event, deref(event.Retryable), tc.class, tc.status, tc.finish, deref(tc.retryable), tc.streamStarted)
			}
			raw, _ := json.Marshal(event)
			if strings.Contains(string(raw), marker) || strings.Contains(string(raw), "test-only-key") {
				t.Fatalf("event carries body or key text: %s", raw)
			}
		})
	}
	t.Run("connection refused", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		if event := failedModelEvent(t, url, nil, ""); event.ErrorClass != ModelCallErrorTransport || event.HTTPStatus != 0 || event.StreamStarted {
			t.Fatalf("refused connection event = %+v", event)
		}
	})
}

func deref(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

// failedModelEvent makes one streamed chat call against url and returns its
// terminal failed (or cancelled) event.
func failedModelEvent(t *testing.T, url string, newCtx func() (context.Context, context.CancelFunc), phase ModelCallPhase) ModelCallEvent {
	t.Helper()
	if phase == "" {
		phase = ModelCallFailed
	}
	ctx, cancel := context.Background(), context.CancelFunc(func() {})
	if newCtx != nil {
		ctx, cancel = newCtx()
	}
	defer cancel()
	provider := llmrouter.NewProviderClient(llmrouter.ProviderConfig{
		ID: "recorded", BaseURL: url, APIKey: "test-only-key", Models: []string{"fixture-model"}, AttemptTimeoutSeconds: 10,
	})
	recorder := newModelObservationTestRecorder()
	model := buildObservedModelLLM(t, &runtimeStreamingLLM{inner: provider}, recorder.observe)
	if _, err := model.Chat(ctx, contract.ChatRequest{Model: "fixture-model", Messages: []contract.Message{{Role: "user", Content: "recorded"}}}); err == nil {
		t.Fatal("recorded failure returned no error")
	}
	return recorder.wait(t, phase)
}

// awaitCancellation consumes the request and starts the stream, so the server
// notices the client going away, then holds the call open until it does.
func awaitCancellation(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}
