package engine

import (
	"io"
	"strings"
	"testing"
	"time"
)

func TestOpenCodePublicEventsArriveBeforeCompletionAndExcludeReasoning(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	observed := make(chan Event, 10)
	done := make(chan openCodeOutput, 1)
	go func() { done <- parseOpenCodeOutputWithEvents(reader, func(event Event, _ bool) { observed <- event }) }()
	_, err := io.WriteString(writer, `{"type":"reasoning","sessionID":"s","part":{"id":"private","type":"reasoning","text":"secret reasoning"}}
{"type":"tool_use","sessionID":"s","part":{"id":"part-tool","type":"tool","callID":"call-1","tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok\n"}}}
`)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-observed:
		if event.Kind != "tool_result" || event.Tool != "bash" || event.CallID != "call-1" || !strings.Contains(event.Input, "go test") || event.Output != "ok\n" {
			t.Fatalf("tool event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("public tool event waited for CLI completion")
	}
	_, err = io.WriteString(writer, `{"type":"text","sessionID":"s","part":{"id":"part-user","role":"user","type":"text","text":"prompt"}}
{"type":"text","sessionID":"s","part":{"id":"part-text","type":"text","text":"测试全部通过"}}
{"type":"step_finish","sessionID":"s","part":{"type":"step-finish","reason":"stop","cost":0,"tokens":{"total":2,"input":1,"output":1,"reasoning":0,"cache":{"read":0,"write":0}}}}
`)
	if err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	result := <-done
	if result.text.String() != "测试全部通过" || result.usage == nil {
		t.Fatalf("terminal result changed: text=%q usage=%v", result.text.String(), result.usage)
	}
	if event := <-observed; event.Kind != "text" || event.Text != "测试全部通过" || event.CallID != "part-text" {
		t.Fatalf("public text = %+v", event)
	}
	select {
	case event := <-observed:
		t.Fatalf("unexpected public event: %+v", event)
	default:
	}
	if !PublishesPublicEvents(OpenCode) || PublishesPublicEvents("loom") {
		t.Fatal("public event engines changed")
	}
}
