package execspec

import (
	"context"
	"reflect"
	"testing"

	"github.com/jinyitao123/weave/internal/base/frozen"
)

func TestFrozenMCPInvocationIsolatedFromCallerMutation(t *testing.T) {
	invocation := FrozenMCPInvocation{
		WorkspaceID: "workspace", AgentID: "agent", AgentVersion: 7, RunSnapshotID: "snapshot",
		FactoryKey: frozen.FactoryKey{FactoryID: "standard", FactoryVersion: "v1"},
		Bindings: []frozen.FrozenMCPBinding{{WorkspaceID: "workspace", ServerID: "tools", ServerRevision: 3,
			Args: []string{"original"}, Filter: []string{"read"}, WriteTools: []string{"write"}}},
	}
	ctx := WithFrozenMCPInvocation(context.Background(), invocation)
	expected := FrozenMCPInvocationFromContext(ctx)
	invocation.AgentID = "other"
	invocation.Bindings[0].ServerID = "other-tools"
	invocation.Bindings[0].Args[0] = "mutated"
	invocation.Bindings[0].Filter[0] = "delete"
	invocation.Bindings[0].WriteTools[0] = "read"
	if actual := FrozenMCPInvocationFromContext(ctx); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("caller mutated frozen authority: %#v", actual)
	}
	returned := FrozenMCPInvocationFromContext(ctx)
	returned.AgentVersion = 99
	returned.Bindings[0].Filter[0] = "all"
	if actual := FrozenMCPInvocationFromContext(ctx); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("reader mutated frozen authority: %#v", actual)
	}
	if actual := FrozenMCPInvocationFromContext(context.Background()); actual != nil {
		t.Fatalf("missing invocation fabricated authority: %#v", actual)
	}
}
