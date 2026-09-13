package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/engine"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSharedRuntimePartitionsUsersAndRejectsForgedPayload(t *testing.T) {
	alice := execution.Subject{WorkspaceID: "shared", UserID: "alice"}
	bob := alice
	bob.UserID = "bob"
	record := &registry.AgentRecord{WorkspaceID: "shared", Name: "worker", ID: "worker-1", Version: 1, Engine: engine.OpenCode}
	makeTask := func(subject execution.Subject) *runtimeprotocol.ExecutionClaim {
		payload, _ := json.Marshal(runtimes.EngineExecRequest{Subject: subject, Agent: record.Name, Engine: record.Engine, Record: record, OneAPIKey: "fixture", Env: map[string]string{"WEAVE_ACTOR_USER_ID": "forged"}})
		return testExecutionClaim(t, &taskqueue.Task{ID: subject.UserID + "-task", Subject: subject, ClaimEpoch: 2, WorkspaceID: subject.WorkspaceID, Agent: record.Name, AgentID: record.ID, AgentVersion: 1, IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeLegacyOrchestrator, Payload: payload})
	}
	var mu sync.Mutex
	dirs := map[string]string{}
	service := &service{workspacesRoot: t.TempDir(), runEngine: func(ctx context.Context, _ string, spec engine.RunSpec) (engine.RunResult, error) {
		actual, err := execution.RequireSubject(ctx, "shared")
		if err != nil || actual != spec.Subject || spec.Env["WEAVE_ACTOR_USER_ID"] != actual.UserID {
			return engine.RunResult{}, errors.New("runtime actor was replaced")
		}
		mu.Lock()
		dirs[actual.UserID] = spec.WorkDir
		mu.Unlock()
		secretFile := filepath.Join(spec.WorkDir, "private.txt")
		if data, err := os.ReadFile(secretFile); err == nil && string(data) != actual.UserID {
			return engine.RunResult{}, errors.New("another user private workdir was reused")
		}
		if err := os.WriteFile(secretFile, []byte(actual.UserID), 0600); err != nil {
			return engine.RunResult{}, err
		}
		return engine.RunResult{Status: "completed", Output: "done"}, nil
	}}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, subject := range []execution.Subject{alice, bob} {
		wg.Add(1)
		go func(subject execution.Subject) {
			defer wg.Done()
			result, err := service.executeTask(context.Background(), makeTask(subject))
			if err == nil && (result.Subject != subject || result.ClaimEpoch != 2) {
				err = errors.New("receipt lost actor or claim epoch")
			}
			failures <- err
		}(subject)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if dirs[alice.UserID] == dirs[bob.UserID] || dirs[alice.UserID] == "" {
		t.Fatal("users share runtime directory")
	}
	if _, err := service.executeTask(execution.WithSubject(context.Background(), bob), makeTask(alice)); err == nil {
		t.Fatal("another user resumed runtime task")
	}
	forged := makeTask(alice)
	forged.Agent.ID = "forged-agent"
	if _, err := service.executeTask(context.Background(), forged); err == nil {
		t.Fatal("payload actor override accepted")
	}
}
