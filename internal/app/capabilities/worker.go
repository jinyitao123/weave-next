package capabilities

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// ServeTasks owns polling of the existing invocation queue. A polling retry
// never grants permission to retry an already-claimed invocation.
func ServeTasks(ctx context.Context, store ExecutionStore, executor TaskExecutor) {
	for ctx.Err() == nil {
		processed, err := RunOne(ctx, store, executor)
		if err != nil && ctx.Err() == nil {
			slog.Error("capability worker failed", "error", fmt.Sprint(err))
		}
		if processed && err == nil {
			continue
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
