package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/labstack/echo/v4"
)

// maxRuntimeLLMBody bounds a proxied ChatRequest. The full session history
// travels with the request (a runtime loom turn must see the same messages a
// server loom turn does), so the cap is generous — but finite, so a runtime
// token can't turn the LLM proxy into an unbounded upload sink.
const maxRuntimeLLMBody = 16 << 20 // 16 MiB

// runtimeLLMStreamFrame is one NDJSON line of a proxied stream. It is a
// StreamChunk plus an out-of-band Error: once the first frame flushes the HTTP
// status is committed to 200, so a mid-stream failure can only be signalled
// in-band. The daemon turns a frame carrying Error into an abnormal turn
// termination rather than treating a truncated stream as complete.
type runtimeLLMStreamFrame struct {
	contract.StreamChunk
	Error string `json:"error,omitempty"`
}

// handleRuntimeTaskLLMChat proxies a single non-streaming model call for a
// remote loom daemon. Like the MCP facade it is authenticated by the runtime
// lease (never a provider key) and is loom-only: CLI engines carry their own
// model credentials and call providers directly. The workspace LLM snapshot is
// resolved server-side from the lease-verified workspace, and the requested
// model is pinned to the task's frozen agent record so a runtime token can only
// reach the models this agent is configured for.
func (s *Server) handleRuntimeTaskLLMChat(c echo.Context) error {
	llm, req, err := s.runtimeLLMRequest(c)
	if err != nil {
		return err
	}
	resp, chatErr := llm.Chat(c.Request().Context(), req)
	if chatErr != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "upstream model error")
	}
	return c.JSON(http.StatusOK, resp)
}

// handleRuntimeTaskLLMStream proxies a streaming model call as newline-delimited
// StreamChunk frames. Client disconnect cancels the request context, which
// cancels the underlying provider stream. A stream that ends without a terminal
// Done frame is reported to the daemon as an in-band error frame.
func (s *Server) handleRuntimeTaskLLMStream(c echo.Context) error {
	llm, req, err := s.runtimeLLMRequest(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	stream, streamErr := llm.Stream(ctx, req)
	if streamErr != nil {
		return echo.NewHTTPError(http.StatusBadGateway, "upstream model error")
	}

	// From here the response is committed to 200; failures are in-band frames,
	// never an HTTP status change.
	c.Response().Header().Set(echo.HeaderContentType, "application/x-ndjson")
	c.Response().WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(c.Response())

	sawDone := false
	for chunk := range stream {
		if chunk.Done {
			sawDone = true
		}
		if encodeErr := encoder.Encode(runtimeLLMStreamFrame{StreamChunk: chunk}); encodeErr != nil {
			// The daemon went away mid-stream; returning lets the request
			// context cancel the upstream provider.
			return nil
		}
		c.Response().Flush()
	}
	// A closed channel with no terminal Done frame means the provider stream
	// ended abnormally. Signal it in-band so the daemon fails the turn.
	if !sawDone {
		_ = encoder.Encode(runtimeLLMStreamFrame{Error: streamAbortReason(ctx)})
		c.Response().Flush()
	}
	return nil
}

// runtimeLLMRequest is the shared prelude for both proxy routes: verify the
// lease and loom-only task, decode a size-bounded ChatRequest, pin the model to
// the frozen record's allowlist, and resolve the workspace LLM snapshot.
func (s *Server) runtimeLLMRequest(c echo.Context) (contract.LLM, contract.ChatRequest, error) {
	runtime, task, err := s.claimedRuntimeTask(c)
	if err != nil {
		return nil, contract.ChatRequest{}, err
	}
	var payload runtimes.EngineExecRequest
	if json.Unmarshal(task.Payload, &payload) != nil {
		return nil, contract.ChatRequest{}, echo.NewHTTPError(http.StatusNotFound, "task not found")
	}
	// The LLM facade is loom-only; CLI engines call providers directly with
	// their own credentials.
	if runtimes.CanonicalEngine(payload.Engine) != runtimes.EngineLoom || payload.Record == nil {
		return nil, contract.ChatRequest{}, echo.NewHTTPError(http.StatusNotFound, "task not found")
	}

	// Bound the request body so a runtime token can't become an unbounded proxy.
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxRuntimeLLMBody)
	var req contract.ChatRequest
	if decodeErr := json.NewDecoder(c.Request().Body).Decode(&req); decodeErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(decodeErr, &tooLarge) {
			return nil, contract.ChatRequest{}, echo.NewHTTPError(http.StatusRequestEntityTooLarge, "chat request too large")
		}
		return nil, contract.ChatRequest{}, echo.NewHTTPError(http.StatusBadRequest, "invalid chat request")
	}

	// The runtime may only invoke a model this agent is configured for. The
	// frozen snapshot — not a live Registry.Get — is authoritative, so an edit
	// after enqueue can't widen a running task's model set.
	model, ok := allowedRuntimeModel(req.Model, payload.Record)
	if !ok {
		return nil, contract.ChatRequest{}, echo.NewHTTPError(http.StatusForbidden, "model not permitted for this task")
	}
	req.Model = model

	llm, err := s.llmFor(c.Request().Context(), runtime.WorkspaceID)
	if err != nil {
		return nil, contract.ChatRequest{}, echo.NewHTTPError(http.StatusServiceUnavailable, "model resolver not configured")
	}
	return llm, req, nil
}

// allowedRuntimeModel resolves the model a proxied call may use: an empty
// request defaults to the record's primary model, and any explicit model must
// be the primary or one of the configured fallbacks.
func allowedRuntimeModel(requested string, rec *registry.AgentRecord) (string, bool) {
	if requested == "" {
		requested = rec.Model
	}
	if requested == "" {
		return "", false
	}
	if requested == rec.Model {
		return requested, true
	}
	for _, fallback := range rec.FallbackModels {
		if requested == fallback {
			return requested, true
		}
	}
	return "", false
}

func streamAbortReason(ctx context.Context) string {
	if err := ctx.Err(); err != nil {
		return "stream cancelled: " + err.Error()
	}
	return "stream ended without completion"
}
