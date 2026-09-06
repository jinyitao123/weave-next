package engine

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestClaudePublicEventsArriveBeforeTerminalAndExcludePrivateBlocks(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	events := make(chan Event, 8)
	done := make(chan claudeOutput, 1)
	go func() { done <- parseClaudeOutputWithEvents(reader, func(event Event, _ bool) { events <- event }) }()
	first := `{"type":"assistant","message":{"id":"msg-1","content":[{"type":"thinking","thinking":"private chain"},{"type":"text","text":"Checking the design"},{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"inputs/baseline.yaml"}}]}}` + "\n"
	if _, err := io.WriteString(writer, first); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"text", "tool_call"} {
		select {
		case event := <-events:
			if event.Kind != kind {
				t.Fatalf("unexpected public event %+v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("public activity waited for terminal output")
		}
	}
	select {
	case <-done:
		t.Fatal("parser finished before terminal event")
	default:
	}
	if _, err := io.WriteString(writer, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"baseline read"}]}}`+"\n"+`{"type":"result","subtype":"success","result":"done","usage":{"input_tokens":1,"output_tokens":1}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	result := <-done
	if result.output != "done" || len(result.events) != 3 || result.events[2].Tool != "Read" || result.events[2].Status != "ok" {
		t.Fatalf("result=%+v", result)
	}
}

func TestClaudeRetryProofRequiresCompleteToolFreeTerminal(t *testing.T) {
	terminal := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"model not found","modelUsage":{"k3[1m]":{}},"usage":{"input_tokens":0,"output_tokens":0}}` + "\n"
	tool := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"one","name":"Write","input":{"file_path":"work.txt"}}]}}` + "\n"
	for _, test := range []struct {
		name, input string
		safe        bool
	}{
		{"definitive rejection", terminal, true},
		{"already executed tool", tool + terminal, false},
		{"incomplete wire", "malformed\n" + terminal, false},
		{"missing terminal", tool, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed := parseClaudeOutput(strings.NewReader(test.input))
			result := claudeRunResult(parsed)
			if result.RetrySafeBeforeExecution != test.safe {
				t.Fatalf("safe=%v", result.RetrySafeBeforeExecution)
			}
			if test.safe && (len(result.ReportedModels) != 1 || result.ReportedModels[0] != "k3[1m]") {
				t.Fatalf("reported models=%v", result.ReportedModels)
			}
		})
	}
}
