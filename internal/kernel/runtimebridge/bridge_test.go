package runtimebridge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/registry"
	"github.com/jinyitao123/weave/internal/kernel/runtimeprotocol"
	"github.com/jinyitao123/weave/internal/kernel/runtimes"
	"github.com/jinyitao123/weave/internal/kernel/taskqueue"
)

func TestClaimSeparatesHostProtocolFromPlatformTask(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Minute)
	subject := execution.Subject{WorkspaceID: "workspace-1", UserID: "user-1"}
	task := &taskqueue.Task{
		ID: "task-1", WorkspaceID: subject.WorkspaceID, Agent: "reviewer", AgentID: "agent-1", AgentVersion: 4,
		IdentityKind: taskqueue.IdentityAgent, IdentitySchemaVersion: 2, ExecutionScope: execution.ScopeTeamWorkerLeaf,
		Kind: "engine_exec", Status: taskqueue.StatusRunning, WorkerID: "runtime:one", Subject: subject, ClaimEpoch: 2,
		UpdatedAt: now, LeaseExpiresAt: &expires, RunSnapshotID: "snapshot-1",
	}
	payload, err := json.Marshal(runtimes.EngineExecRequest{Subject: subject, Engine: "codex", Prompt: "review", Record: &registry.AgentRecord{WorkspaceID: subject.WorkspaceID, ID: task.AgentID, Version: task.AgentVersion, Name: task.Agent}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := Claim(task, payload)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Request.SchemaVersion != runtimeprotocol.RequestSchemaV1 || claim.Request.Engine != "codex" || claim.Agent.ID != task.AgentID || claim.Subject != subject {
		t.Fatalf("claim=%+v", claim)
	}
	var frozen map[string]any
	if err := json.Unmarshal(claim.Request.FrozenAgent, &frozen); err != nil || frozen["id"] != task.AgentID {
		t.Fatalf("frozen=%v err=%v", frozen, err)
	}
}

func TestClaimRejectsUnclaimedPlatformRecord(t *testing.T) {
	if _, err := Claim(&taskqueue.Task{Kind: "engine_exec", Status: taskqueue.StatusQueued}, json.RawMessage(`{}`)); err == nil {
		t.Fatal("queued platform record crossed the Host boundary")
	}
}
