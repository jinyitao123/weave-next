package engine

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCodexPublicEventsArriveBeforeCompletionAndExcludeReasoning(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	observed := make(chan Event, 10)
	done := make(chan codexOutput, 1)
	go func() { done <- parseCodexOutputWithEvents(reader, func(event Event, _ bool) { observed <- event }) }()
	_, err := io.WriteString(writer, `{"type":"item.started","item":{"id":"private","type":"reasoning","text":"secret reasoning"}}
{"type":"item.updated","item":{"id":"private","type":"reasoning","text":"more private reasoning"}}
{"type":"item.started","item":{"id":"call","type":"command_execution","command":"pwd"}}
`)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-observed:
		if event.Kind != "tool_call" || event.Tool != "shell" {
			t.Fatalf("non-public event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("public tool event waited for CLI completion")
	}
	select {
	case <-done:
		t.Fatal("parser finished before stream ended")
	default:
	}
	_, err = io.WriteString(writer, `{"type":"item.updated","item":{"id":"public","type":"agent_message","text":"已整理材料"}}
{"type":"item.completed","item":{"id":"public","type":"agent_message","text":"已整理材料"}}
{"type":"item.completed","item":{"id":"call","type":"command_execution","command":"pwd","aggregated_output":"workspace","exit_code":0}}
{"type":"item.completed","item":{"id":"private","type":"reasoning","text":"private result"}}
{"type":"unknown.future","item":{"type":"agent_message","text":"unknown protocol"}}
{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1}}
`)
	if err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	result := <-done
	if result.output != "已整理材料" || len(result.events) != 2 {
		t.Fatalf("terminal result changed: %+v", result)
	}
	if event := <-observed; event.Kind != "text" || event.Text != result.output {
		t.Fatalf("public text missing: %+v", event)
	}
	if event := <-observed; event.Kind != "tool_result" || event.Output != "workspace" {
		t.Fatalf("tool result missing: %+v", event)
	}
	select {
	case event := <-observed:
		t.Fatalf("duplicate/private/unknown event leaked: %+v", event)
	default:
	}
}

func TestCodexPublicTextBoundIsUTF8AndExplicit(t *testing.T) {
	text := strings.Repeat("材", 2000)
	line, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]string{"type": "agent_message", "text": text}})
	count := 0
	result := parseCodexOutputWithEvents(strings.NewReader(string(line)), func(event Event, truncated bool) {
		count++
		if !truncated || !utf8.ValidString(event.Text) || len(event.Text) > 4096 || event.Kind != "text" {
			t.Fatalf("invalid bounded public text: %d %v", len(event.Text), truncated)
		}
	})
	if count != 1 || result.output != text {
		t.Fatal("progress clipping changed authoritative final text")
	}
}
