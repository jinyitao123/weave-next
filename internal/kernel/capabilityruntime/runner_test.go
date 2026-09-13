package capabilityruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/capability"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/execenv"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
)

type testRemote struct {
	call func(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, json.RawMessage) (engine.RunResult, error)
}

func (e testRemote) ExecRemote(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, []execenv.Attachment) (engine.RunResult, error) {
	return engine.RunResult{}, errors.New("unexpected unstructured execution")
}

func (e testRemote) ExecRemoteStructured(ctx context.Context, ws string, rec *registry.AgentRecord, stamp execution.AgentExecutionStamp, prompt string, _ []execenv.Attachment, schema json.RawMessage) (engine.RunResult, error) {
	return e.call(ctx, ws, rec, stamp, prompt, schema)
}

type testRecorder struct {
	runtimeID string
	runs      map[string]string
	events    []capability.ExecutionEvent
	bindErr   error
}

func (r *testRecorder) BindRuntime(_ context.Context, id string) error {
	r.runtimeID = id
	return r.bindErr
}
func (r *testRecorder) RecordRun(_ context.Context, step, run string) error {
	if r.runs == nil {
		r.runs = map[string]string{}
	}
	r.runs[step] = run
	return nil
}
func (r *testRecorder) Checkpoint(_ context.Context, _ capability.ExecutionState, event capability.ExecutionEvent) error {
	r.events = append(r.events, event)
	return nil
}

func remoteRequest() Request {
	return Request{RunKind: "published", WorkspaceID: "ws", InvocationID: "inv",
		Input: json.RawMessage(`{"material":"original"}`),
		Plan: capability.Plan{
			Runtime:     capability.RuntimeRequirement{Engine: "codex", Pool: "pool"},
			InputSchema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`),
			Steps: []capability.PlanStep{{ID: "review", Kind: capability.StepWorker, RoleName: "Reviewer", RoleDescription: "Check material", Instruction: "Review the source", OutputSchema: json.RawMessage(`{"type":"object","required":["ok"]}`)}},
		}}
}

func testRunner(t *testing.T, remote testRemote) *Runner {
	t.Helper()
	return &Runner{Remote: remote, ListRuntimes: func(_ context.Context, ws string) ([]runtimes.Runtime, error) {
		if ws != "ws" {
			t.Errorf("runtime lookup escaped workspace: %q", ws)
		}
		return []runtimes.Runtime{
			{ID: "busy", Enabled: true, Online: true, PoolID: "pool", Engines: []string{"codex"}, TotalSlots: 1, ActiveSlots: 1},
			{ID: "elsewhere", Enabled: true, Online: true, PoolID: "other", Engines: []string{"codex"}, TotalSlots: 1},
			{ID: "chosen", Enabled: true, Online: true, PoolID: "pool", Engines: []string{"codex", "claude", "opencode"}, TotalSlots: 1},
		}, nil
	}}
}

func TestRunnerPreservesRemoteIdentityInputAndDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	for _, engineName := range []string{"codex", "claude", "opencode"} {
		t.Run(engineName, func(t *testing.T) {
			calls := 0
			runner := testRunner(t, testRemote{call: func(got context.Context, ws string, rec *registry.AgentRecord, stamp execution.AgentExecutionStamp, prompt string, schema json.RawMessage) (engine.RunResult, error) {
				calls++
				actual, _ := got.Deadline()
				if actual != deadline || ws != "ws" || rec.Engine != engineName || rec.RuntimeID != "chosen" || rec.RuntimePolicyMode != "strict_pin" {
					t.Errorf("lost execution binding: %+v deadline=%v", rec, actual)
				}
				if stamp.AgentID != rec.ID || stamp.RunSnapshotID != "inv" || stamp.ExecutionScope != execution.ScopeTeamWorkerLeaf {
					t.Errorf("lost execution identity: %+v", stamp)
				}
				if !strings.Contains(prompt, "original") || !strings.Contains(prompt, "Reviewer") || !strings.Contains(string(schema), "ok") {
					t.Error("input, role or output contract lost")
				}
				return engine.RunResult{Output: `{"ok":true}`, SessionID: "session"}, nil
			}})
			request := remoteRequest()
			request.Plan.Runtime.Engine = engineName
			recorder := &testRecorder{}
			result, err := runner.Execute(ctx, request, recorder)
			if err != nil || calls != 1 || string(result) != `{"review":{"ok":true}}` || recorder.runtimeID != "chosen" || recorder.runs["review"] != "session" {
				t.Fatalf("result=%s calls=%d recorder=%+v err=%v", result, calls, recorder, err)
			}
			if len(recorder.events) != 2 || recorder.events[1].Type != "execution_completed" {
				t.Fatalf("checkpoint events=%+v", recorder.events)
			}
		})
	}
}

func TestRunnerDoesNotRedispatchOnFailureOrLostClaim(t *testing.T) {
	failure := errors.New("execution result unknown")
	for _, lostClaim := range []bool{false, true} {
		calls := 0
		runner := testRunner(t, testRemote{call: func(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, json.RawMessage) (engine.RunResult, error) {
			calls++
			return engine.RunResult{}, failure
		}})
		recorder := &testRecorder{}
		if lostClaim {
			recorder.bindErr = failure
		}
		_, err := runner.Execute(t.Context(), remoteRequest(), recorder)
		wantCalls := 1
		if lostClaim {
			wantCalls = 0
		}
		if !errors.Is(err, failure) || calls != wantCalls || len(recorder.events) != 0 {
			t.Fatalf("lostClaim=%v calls=%d err=%v events=%v", lostClaim, calls, err, recorder.events)
		}
	}
}

func TestRunnerPropagatesCancellationWithoutCompleting(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	runner := testRunner(t, testRemote{call: func(ctx context.Context, _ string, _ *registry.AgentRecord, _ execution.AgentExecutionStamp, _ string, _ json.RawMessage) (engine.RunResult, error) {
		close(started)
		<-ctx.Done()
		return engine.RunResult{}, ctx.Err()
	}})
	recorder := &testRecorder{}
	done := make(chan error, 1)
	go func() { _, err := runner.Execute(ctx, remoteRequest(), recorder); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || len(recorder.events) != 0 {
		t.Fatalf("cancel err=%v events=%v", err, recorder.events)
	}
}

func TestRunnerResumeDoesNotReexecuteConfirmedStep(t *testing.T) {
	runner := testRunner(t, testRemote{call: func(context.Context, string, *registry.AgentRecord, execution.AgentExecutionStamp, string, json.RawMessage) (engine.RunResult, error) {
		t.Error("confirmed work executed again")
		return engine.RunResult{}, errors.New("unexpected execution")
	}})
	request := remoteRequest()
	request.State.Outputs = map[string]json.RawMessage{"review": json.RawMessage(`{"ok":true}`)}
	result, err := runner.Execute(t.Context(), request, &testRecorder{})
	if err != nil || string(result) != `{"review":{"ok":true}}` {
		t.Fatalf("resume result=%s err=%v", result, err)
	}
}
