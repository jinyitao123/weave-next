package execution

import (
	"context"
	"testing"
)

func TestOperationIdentityIsStableAndScoped(t *testing.T) {
	ctx := WithOperationID(context.Background(), "member/segment/000000000002")
	key := EngineOperationID("input-a", "invocation-a", OperationID(ctx), "capability-a")
	if len(key) != 73 || key != EngineOperationID("input-a", "invocation-a", OperationID(ctx), "capability-a") {
		t.Fatal("operation key must be stable and bounded")
	}
	for _, args := range [][4]string{{"input-b", "invocation-a", OperationID(ctx), "capability-a"}, {"input-a", "invocation-b", OperationID(ctx), "capability-a"}, {"input-a", "invocation-a", "member/segment/000000000003", "capability-a"}, {"input-a", "invocation-a", OperationID(ctx), "capability-b"}} {
		if key == EngineOperationID(args[0], args[1], args[2], args[3]) {
			t.Fatal("operation scope crossed")
		}
	}
	if OperationID(context.Background()) != "" || EngineOperationID("input-a", "invocation-a", "", "capability-a") != "" {
		t.Fatal("missing durable slot must not invent identity")
	}
}
