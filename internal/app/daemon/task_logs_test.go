package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

func TestTaskLogsShipEveryLineInOrder(t *testing.T) {
	var mu sync.Mutex
	received := []runtimeprotocol.TaskLogLine{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runtime/tasks/task-1/logs" || r.Header.Get("X-Weave-Task-Epoch") != "3" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		var request runtimeprotocol.TaskLogRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Lines) > runtimeprotocol.TaskLogBatchLimit {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, request.Lines...)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := newRuntimeClient(server.URL, "rtk_test", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	claim := &runtimeprotocol.ExecutionClaim{TaskID: "task-1", ClaimEpoch: 3, Subject: execution.Subject{WorkspaceID: "ws", UserID: "u"}}
	logs := startTaskLogs(context.Background(), client, claim)
	logs.event(engine.Event{Kind: "text", Text: "first\nsecond"})
	logs.event(engine.Event{Kind: "tool_call", Tool: "bash", Input: "go test ./..."})
	logs.event(engine.Event{Kind: "thinking", Text: "never logged"})
	for index := 0; index < 450; index++ {
		logs.write("command-1", "ok")
	}
	writer := &lineWriter{logs: logs, stream: "command-2"}
	_, _ = writer.Write([]byte("partial "))
	_, _ = writer.Write([]byte("line\nnext"))
	writer.close()
	logs.write("command-3", strings.Repeat("界", runtimeprotocol.TaskLogLineBytes))
	logs.close()

	byStream := map[string][]runtimeprotocol.TaskLogLine{}
	for _, line := range received {
		byStream[line.Stream] = append(byStream[line.Stream], line)
	}
	agent := byStream["agent"]
	if len(agent) != 3 || agent[0].Text != "first" || agent[1].Text != "second" || agent[2].Text != "▸ bash go test ./..." || agent[2].Seq != 3 {
		t.Fatalf("agent stream = %+v", agent)
	}
	if commands := byStream["command-1"]; len(commands) != 450 || commands[449].Seq != 450 {
		t.Fatalf("command stream lost lines: %d", len(byStream["command-1"]))
	}
	if second := byStream["command-2"]; len(second) != 2 || second[0].Text != "partial line" || second[1].Text != "next" {
		t.Fatalf("split lines = %+v", second)
	}
	long := byStream["command-3"]
	if len(long) != 1 || len(long[0].Text) > runtimeprotocol.TaskLogLineBytes || runtimeprotocol.ValidateTaskLogLine(long[0]) != nil {
		t.Fatalf("long line was not bounded on a rune boundary: %d", len(long[0].Text))
	}
}
