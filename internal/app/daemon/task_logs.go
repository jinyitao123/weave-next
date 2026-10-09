package daemon

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
)

// taskLogs ships every output line of one execution to the server in small
// batches. It is best effort: the authoritative result never waits on it, and
// a task stops logging at the protocol ceiling.
type taskLogs struct {
	client  *runtimeClient
	ctx     context.Context
	taskID  string
	mu      sync.Mutex
	pending []runtimeprotocol.TaskLogLine
	seq     map[string]int64
	total   int
	wake    chan struct{}
	done    chan struct{}
	stopped chan struct{}
}

const taskLogFlushInterval = 500 * time.Millisecond

func (c *runtimeClient) taskLogs(ctx context.Context, taskID string, lines []runtimeprotocol.TaskLogLine) error {
	response, err := c.do(ctx, http.MethodPost, "/v1/runtime/tasks/"+url.PathEscape(taskID)+"/logs", runtimeprotocol.TaskLogRequest{Versioned: runtimeprotocol.NewVersioned(), Lines: lines})
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return expectStatus(response, http.StatusNoContent)
}

func startTaskLogs(ctx context.Context, client *runtimeClient, task *runtimeprotocol.ExecutionClaim) *taskLogs {
	if client == nil || task == nil {
		return nil
	}
	logs := &taskLogs{
		client: client, ctx: withTaskProof(context.WithoutCancel(ctx), task.Subject, task.ClaimEpoch), taskID: task.TaskID,
		seq: map[string]int64{}, wake: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{}),
	}
	go logs.loop()
	return logs
}

// write records text as one or more lines of a stream.
func (logs *taskLogs) write(stream, text string) {
	if logs == nil {
		return
	}
	text = strings.ToValidUTF8(strings.TrimRight(text, "\r\n"), "�")
	logs.mu.Lock()
	for _, line := range strings.Split(text, "\n") {
		if logs.total >= runtimeprotocol.TaskLogMaxLines {
			break
		}
		line = strings.TrimRight(line, "\r")
		if len(line) > runtimeprotocol.TaskLogLineBytes {
			cut := runtimeprotocol.TaskLogLineBytes
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut]
		}
		logs.seq[stream]++
		logs.total++
		logs.pending = append(logs.pending, runtimeprotocol.TaskLogLine{Stream: stream, Seq: logs.seq[stream], OccurredAt: time.Now().UTC(), Text: line})
	}
	full := len(logs.pending) >= runtimeprotocol.TaskLogBatchLimit
	logs.mu.Unlock()
	if full {
		select {
		case logs.wake <- struct{}{}:
		default:
		}
	}
}

// event records one public member event in the agent stream.
func (logs *taskLogs) event(event engine.Event) {
	switch event.Kind {
	case "text":
		logs.write("agent", event.Text)
	case "tool_call":
		logs.write("agent", "▸ "+strings.TrimSpace(event.Tool+" "+event.Input))
	case "tool_result":
		logs.write("agent", "◂ "+event.Output)
	}
}

func (logs *taskLogs) flush() {
	for {
		logs.mu.Lock()
		if len(logs.pending) == 0 {
			logs.mu.Unlock()
			return
		}
		batch := logs.pending
		if len(batch) > runtimeprotocol.TaskLogBatchLimit {
			batch = batch[:runtimeprotocol.TaskLogBatchLimit]
		}
		logs.mu.Unlock()
		ctx, cancel := context.WithTimeout(logs.ctx, 10*time.Second)
		err := logs.client.taskLogs(ctx, logs.taskID, batch)
		cancel()
		if err != nil {
			return // retried on the next tick; lines are idempotent by sequence
		}
		logs.mu.Lock()
		logs.pending = logs.pending[len(batch):]
		logs.mu.Unlock()
	}
}

func (logs *taskLogs) loop() {
	defer close(logs.stopped)
	ticker := time.NewTicker(taskLogFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-logs.done:
			logs.flush()
			return
		case <-ticker.C:
			logs.flush()
		case <-logs.wake:
			logs.flush()
		}
	}
}

// close sends what remains, waiting a bounded time.
func (logs *taskLogs) close() {
	if logs == nil {
		return
	}
	close(logs.done)
	select {
	case <-logs.stopped:
	case <-time.After(15 * time.Second):
	}
}

// lineWriter turns a byte stream into log lines.
type lineWriter struct {
	logs    *taskLogs
	stream  string
	partial []byte
}

func (writer *lineWriter) Write(data []byte) (int, error) {
	writer.partial = append(writer.partial, data...)
	for {
		index := strings.IndexByte(string(writer.partial), '\n')
		if index < 0 {
			break
		}
		writer.logs.write(writer.stream, string(writer.partial[:index]))
		writer.partial = writer.partial[index+1:]
	}
	if len(writer.partial) > runtimeprotocol.TaskLogLineBytes {
		writer.logs.write(writer.stream, string(writer.partial))
		writer.partial = nil
	}
	return len(data), nil
}

func (writer *lineWriter) close() {
	if len(writer.partial) > 0 {
		writer.logs.write(writer.stream, string(writer.partial))
		writer.partial = nil
	}
}
