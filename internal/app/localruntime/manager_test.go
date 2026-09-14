package localruntime

import (
	"testing"

	"github.com/jinyitao123/weave/internal/base/execution"
	"github.com/jinyitao123/weave/internal/kernel/registry"
)

func TestManagedSelectionNeverReplacesFrozenRuntime(t *testing.T) {
	manager := &Manager{}
	pinned := &registry.AgentRecord{RuntimeID: "explicit-remote"}
	stamp := execution.AgentExecutionStamp{RunSnapshotID: "published-run", ExecutionScope: execution.ScopeTeamWorkerLeaf}
	bound, err := manager.bindRecord(t.Context(), "workspace", pinned, stamp)
	if err != nil || bound != pinned || bound.RuntimeID != "explicit-remote" {
		t.Fatalf("published runtime was replaced: %+v %v", bound, err)
	}
	if _, err := manager.bindRecord(t.Context(), "workspace", &registry.AgentRecord{}, stamp); err == nil {
		t.Fatal("unbound frozen agent silently chose a machine")
	}
}

func TestManagedMachineHasOneProcessOwner(t *testing.T) {
	root := t.TempDir()
	release, err := lockHostRoot(root)
	if err != nil {
		t.Skip("platform does not support an embedded Host")
	}
	defer release()
	if second, err := lockHostRoot(root); err == nil {
		second()
		t.Fatal("two managers own one Host identity and spool")
	}
}
