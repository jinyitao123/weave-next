package execution

import (
	"context"
	"errors"
	"strings"
)

// CurrentTask is the durable claim currently executing on the platform. It is
// control authority for child admission, never a model argument or attempt
// lineage reference. Queue adapters verify it against their stored claim both
// before binding and again when admitting a child.
type CurrentTask struct {
	ID          string
	WorkspaceID string
	Subject     Subject
	WorkerID    string
	ClaimEpoch  int64
}

var ErrCurrentTaskMismatch = errors.New("current execution task does not match its claim")

type currentTaskKey struct{}

func WithCurrentTask(ctx context.Context, task CurrentTask) (context.Context, error) {
	subject, err := RequireSubject(ctx, task.WorkspaceID)
	if err != nil {
		return nil, err
	}
	if task.Subject != subject || task.ID == "" || strings.TrimSpace(task.ID) != task.ID ||
		task.WorkerID == "" || strings.TrimSpace(task.WorkerID) != task.WorkerID || task.ClaimEpoch < 1 {
		return nil, ErrCurrentTaskMismatch
	}
	return context.WithValue(ctx, currentTaskKey{}, task), nil
}

func CurrentTaskFromContext(ctx context.Context) (CurrentTask, bool) {
	task, ok := ctx.Value(currentTaskKey{}).(CurrentTask)
	return task, ok
}
