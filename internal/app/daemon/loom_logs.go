package daemon

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/engine"
)

// A loom turn on a node has no CLI stdout, so its log comes from the two ports
// it already crosses: model replies and tool calls. Logging never changes what
// either port returns.
const loomLogTextLimit = 4096

type loggedLoomModel struct {
	next contract.LLM
	logs *taskLogs
}

func (model loggedLoomModel) Chat(ctx context.Context, req contract.ChatRequest) (*contract.ChatResponse, error) {
	response, err := model.next.Chat(ctx, req)
	if err == nil && response != nil && strings.TrimSpace(response.Content) != "" {
		model.logs.event(engine.Event{Kind: "text", Text: boundedLoomText(response.Content)})
	}
	return response, err
}

func (model loggedLoomModel) Stream(ctx context.Context, req contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	upstream, err := model.next.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	chunks := make(chan contract.StreamChunk)
	go func() {
		defer close(chunks)
		var text strings.Builder
		defer func() {
			if strings.TrimSpace(text.String()) != "" {
				model.logs.event(engine.Event{Kind: "text", Text: boundedLoomText(text.String())})
			}
		}()
		for chunk := range upstream {
			if text.Len() < loomLogTextLimit {
				text.WriteString(chunk.Content)
			}
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				// Drain so the upstream producer can exit.
				for range upstream {
				}
				return
			}
		}
	}()
	return chunks, nil
}

type loggedLoomTools struct {
	next contract.ToolDispatcher
	logs *taskLogs
}

func (tools loggedLoomTools) ListTools(ctx context.Context) ([]contract.ToolDef, error) {
	return tools.next.ListTools(ctx)
}

func (tools loggedLoomTools) Dispatch(ctx context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	tools.logs.event(engine.Event{Kind: "tool_call", Tool: call.Name, Input: boundedLoomText(call.Args)})
	result, err := tools.next.Dispatch(ctx, call)
	switch {
	case err != nil:
		tools.logs.event(engine.Event{Kind: "tool_result", Output: "工具调用失败"})
	case result != nil && result.Park:
		tools.logs.event(engine.Event{Kind: "tool_result", Output: "等待批准"})
	case result != nil:
		tools.logs.event(engine.Event{Kind: "tool_result", Output: boundedLoomText(result.Content)})
	}
	return result, err
}

func boundedLoomText(value string) string {
	if len(value) <= loomLogTextLimit {
		return value
	}
	value = value[:loomLogTextLimit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
