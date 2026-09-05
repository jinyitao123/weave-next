package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/base/taskqueue"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

func TestRuntimeRecoveryWaitsForProcessExitBeforeAcknowledgement(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop_request", true: "hung_connection"}[cut], func(t *testing.T) {
			var exited, ack atomic.Bool
			var acknowledgements atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/renew"):
					if cut {
						<-r.Context().Done()
						return
					}
					w.WriteHeader(http.StatusConflict)
				case strings.HasSuffix(r.URL.Path, "/stopped"):
					if !exited.Load() {
						t.Error("acknowledged before execution exit")
					}
					if acknowledgements.Add(1) == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					ack.Store(true)
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			client, err := newRuntimeClient(server.URL, "fixture", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			rec := &registry.AgentRecord{Name: "worker", ID: "agent-1", WorkspaceID: "workspace-1", Version: 1, Engine: engine.OpenCode}
			payload, _ := json.Marshal(runtimes.EngineExecRequest{Record: rec, Agent: rec.Name, Engine: rec.Engine, Prompt: "fixture"})
			now := time.Now()
			expiry := now.Add(120 * time.Millisecond)
			task := &taskqueue.Task{ID: "task-1", WorkspaceID: rec.WorkspaceID, Agent: rec.Name, AgentID: rec.ID, AgentVersion: 1,
				IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator,
				Payload: payload, UpdatedAt: now, LeaseExpiresAt: &expiry}
			d := &service{client: client, server: server.URL, workspacesRoot: t.TempDir(), renewInterval: time.Millisecond,
				minBackoff: time.Millisecond, maxBackoff: 2 * time.Millisecond, runEngine: func(ctx context.Context, _ string, _ engine.RunSpec) (engine.RunResult, error) {
					<-ctx.Done()
					time.Sleep(10 * time.Millisecond)
					exited.Store(true)
					return engine.RunResult{}, ctx.Err()
				}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			d.processTask(ctx, task)
			if ctx.Err() != nil || !exited.Load() || !ack.Load() || acknowledgements.Load() != 2 {
				t.Fatalf("exit=%v ack=%v attempts=%d err=%v", exited.Load(), ack.Load(), acknowledgements.Load(), ctx.Err())
			}
		})
	}
}

func TestRuntimeRecoveryLostCompletionResponseDoesNotExecuteAgain(t *testing.T) {
	var calls, executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/complete") && calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/complete") {
			w.WriteHeader(http.StatusConflict)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/stopped") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		t.Errorf("unexpected endpoint %s", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, _ := newRuntimeClient(server.URL, "fixture", server.Client())
	rec := &registry.AgentRecord{Name: "worker", ID: "agent-1", WorkspaceID: "workspace-1", Version: 1, Engine: engine.OpenCode}
	payload, _ := json.Marshal(runtimes.EngineExecRequest{Record: rec, Agent: rec.Name, Engine: rec.Engine, Prompt: "fixture"})
	task := &taskqueue.Task{ID: "task-1", WorkspaceID: rec.WorkspaceID, Agent: rec.Name, AgentID: rec.ID, AgentVersion: 1, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Payload: payload}
	d := &service{client: client, server: server.URL, workspacesRoot: t.TempDir(), renewInterval: time.Second, minBackoff: time.Millisecond, maxBackoff: time.Millisecond,
		runEngine: func(context.Context, string, engine.RunSpec) (engine.RunResult, error) {
			executions.Add(1)
			return engine.RunResult{Status: "completed", Output: "saved"}, nil
		}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.processTask(ctx, task)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || executions.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("executions=%d reports=%d err=%v", executions.Load(), calls.Load(), ctx.Err())
	}
}
