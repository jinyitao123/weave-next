package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jinyitao123/loom/contract"
)

type scriptedLoomModel struct{ reply string }

func (model scriptedLoomModel) Chat(context.Context, contract.ChatRequest) (*contract.ChatResponse, error) {
	return &contract.ChatResponse{Content: model.reply}, nil
}

func (model scriptedLoomModel) Stream(context.Context, contract.ChatRequest) (<-chan contract.StreamChunk, error) {
	chunks := make(chan contract.StreamChunk, 3)
	chunks <- contract.StreamChunk{Content: "分析"}
	chunks <- contract.StreamChunk{Content: "完成"}
	chunks <- contract.StreamChunk{Done: true}
	close(chunks)
	return chunks, nil
}

type scriptedLoomTools struct{ fail bool }

func (scriptedLoomTools) ListTools(context.Context) ([]contract.ToolDef, error) { return nil, nil }

func (tools scriptedLoomTools) Dispatch(_ context.Context, call contract.ToolCall) (*contract.ToolResult, error) {
	if tools.fail {
		return nil, errors.New("upstream detail")
	}
	return &contract.ToolResult{CallID: call.ID, Content: "3 files"}, nil
}

func TestLoomPortsWriteTheTaskLogWithoutChangingResults(t *testing.T) {
	logs := &taskLogs{seq: map[string]int64{}}
	ctx := context.Background()
	model := loggedLoomModel{next: scriptedLoomModel{reply: "先看目录"}, logs: logs}
	if response, err := model.Chat(ctx, contract.ChatRequest{}); err != nil || response.Content != "先看目录" {
		t.Fatalf("chat = %+v %v", response, err)
	}
	stream, err := model.Stream(ctx, contract.ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var streamed []contract.StreamChunk
	for chunk := range stream {
		streamed = append(streamed, chunk)
	}
	if len(streamed) != 3 || !streamed[2].Done {
		t.Fatalf("stream changed: %+v", streamed)
	}
	tools := loggedLoomTools{next: scriptedLoomTools{}, logs: logs}
	if result, err := tools.Dispatch(ctx, contract.ToolCall{ID: "c", Name: "list_files", Args: `{"dir":"."}`}); err != nil || result.Content != "3 files" {
		t.Fatalf("dispatch = %+v %v", result, err)
	}
	failing := loggedLoomTools{next: scriptedLoomTools{fail: true}, logs: logs}
	if _, err := failing.Dispatch(ctx, contract.ToolCall{Name: "list_files"}); err == nil {
		t.Fatal("tool error was swallowed")
	}
	var lines []string
	for _, line := range logs.pending {
		if line.Stream != "agent" {
			t.Fatalf("stream = %q", line.Stream)
		}
		lines = append(lines, line.Text)
	}
	want := []string{"先看目录", "分析完成", `▸ list_files {"dir":"."}`, "◂ 3 files", "▸ list_files", "◂ 工具调用失败"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("log lines = %q", lines)
	}
}
