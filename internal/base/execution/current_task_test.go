package execution

import (
	"context"
	"testing"
)

func TestCurrentTaskDoesNotInheritAttemptLineageOrCrossSubject(t *testing.T) {
	subject := Subject{WorkspaceID: "workspace", UserID: "alice"}
	ctx := WithSubject(context.Background(), subject)
	ctx = WithAttemptLineage(ctx, "trace", "previous-attempt")
	if _, ok := CurrentTaskFromContext(ctx); ok {
		t.Fatal("attempt correlation created control authority")
	}
	task := CurrentTask{ID: "control", WorkspaceID: subject.WorkspaceID, Subject: subject, WorkerID: "owner", ClaimEpoch: 3}
	bound, err := WithCurrentTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	observed, _ := CurrentTaskFromContext(WithAttemptLineage(bound, "another-trace", "another-attempt"))
	if observed != task {
		t.Fatal("attempt lineage replaced control authority")
	}
	for _, invalid := range []CurrentTask{
		{ID: "control", WorkspaceID: "other", Subject: subject, WorkerID: "owner", ClaimEpoch: 3},
		{ID: "control", WorkspaceID: "workspace", Subject: Subject{WorkspaceID: "workspace", UserID: "bob"}, WorkerID: "owner", ClaimEpoch: 3},
		{ID: "control", WorkspaceID: "workspace", Subject: subject, ClaimEpoch: 3},
		{ID: "control", WorkspaceID: "workspace", Subject: subject, WorkerID: "owner"},
	} {
		if _, err := WithCurrentTask(ctx, invalid); err == nil {
			t.Fatalf("invalid claim bound: %+v", invalid)
		}
	}
	if _, err := WithCurrentTask(context.Background(), task); err == nil {
		t.Fatal("missing authenticated subject accepted")
	}
}
