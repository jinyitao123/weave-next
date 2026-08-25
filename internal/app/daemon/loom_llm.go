package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/jinyitao123/loom/contract"
)

// maxRuntimeLLMStreamLine bounds one NDJSON stream frame. Content frames are
// token-sized and the terminal frame only adds usage, so a megabyte is far
// beyond any legitimate frame while still refusing an unbounded line.
const maxRuntimeLLMStreamLine = 1 << 20

// runtimeLLMStreamWireFrame mirrors the server's per-line stream frame
// (internal/api.runtimeLLMStreamFrame): a StreamChunk plus an out-of-band Error
// the server sets when the upstream stream ends abnormally. The daemon turns a
// frame carrying Error into an early channel close so a consumer never mistakes
// a truncated stream for a completed one.
type runtimeLLMStreamWireFrame struct {
	contract.StreamChunk
	Error string `json:"error,omitempty"`
}

// runtimeLLMClient is a contract.LLM bound to one claimed task. Every model call
// is proxied back to the server, which holds the provider credentials — the
// daemon never sees an API key. Satisfying loom's LLM interface is what lets a
// loom run on the edge be the same graph a server run would build; the credential
// boundary is the only difference, and it stays on the server.
type runtimeLLMClient struct {
	client *runtimeClient
	taskID string
}

func newRuntimeLLMClient(client *runtimeClient, taskID string) *runtimeLLMClient {
	return &runtimeLLMClient{client: client, taskID: taskID}
}

func (l *runtimeLLMClient) chatPath() string {
	return "/v1/runtime/tasks/" + url.PathEscape(l.taskID) + "/llm/chat"
}

func (l *runtimeLLMClient) streamPath() string {
	return "/v1/runtime/tasks/" + url.PathEscape(l.taskID) + "/llm/stream"
}

// Chat is the call loom's ToolLoop actually drives; its error surfaces as the
// turn error, so a proxy failure fails the run rather than silently degrading.
func (l *runtimeLLMClient) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	response, err := l.client.do(ctx, http.MethodPost, l.chatPath(), req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := expectStatus(response, http.StatusOK); err != nil {
		return nil, err
	}
	var chat contract.ChatResponse
	if err := json.NewDecoder(response.Body).Decode(&chat); err != nil {
		return nil, fmt.Errorf("daemon: decode chat response: %w", err)
	}
	return &chat, nil
}

// Stream proxies the server's NDJSON stream back into a StreamChunk channel. An
// upfront non-200 is a normal error; a mid-stream error frame or a malformed
// line closes the channel without a terminal Done, so no consumer treats a
// truncated stream as complete.
func (l *runtimeLLMClient) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	response, err := l.client.do(ctx, http.MethodPost, l.streamPath(), req)
	if err != nil {
		return nil, err
	}
	if err := expectStatus(response, http.StatusOK); err != nil {
		response.Body.Close()
		return nil, err
	}
	chunks := make(chan contract.StreamChunk)
	go func() {
		defer response.Body.Close()
		defer close(chunks)
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), maxRuntimeLLMStreamLine)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var frame runtimeLLMStreamWireFrame
			if err := json.Unmarshal(line, &frame); err != nil {
				return
			}
			if frame.Error != "" {
				return
			}
			select {
			case chunks <- frame.StreamChunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return chunks, nil
}
