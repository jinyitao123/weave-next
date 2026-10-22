package workflow

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCompactModelObservationsKeepPhasesAndExposeSlowCalls(t *testing.T) {
	ctx := context.Background()
	var events []ModelCallEvent
	call := newModelCallWithOptions(ctx, "m", func(_ context.Context, e ModelCallEvent) { events = append(events, e) }, ModelObservationOptions{CompactPhases: true})
	call.emit(ctx, ModelCallStarted, "")
	call.emit(ctx, ModelCallRequestWritten, "")
	call.emit(ctx, ModelCallFirstResponseByte, "")
	call.finish(ctx, nil)
	call.emit(ctx, ModelCallFirstChunk, "") // a late transport callback is ignored
	if len(events) != 2 || events[1].Phase != ModelCallCompleted || len(events[1].Phases) != 2 {
		t.Fatalf("compact lifecycle: %+v", events)
	}
	var mu sync.Mutex
	visible := make(chan struct{}, 1)
	slow := newModelCallWithOptions(ctx, "m", func(_ context.Context, e ModelCallEvent) {
		mu.Lock()
		defer mu.Unlock()
		if e.Phase == ModelCallRequestWritten {
			visible <- struct{}{}
		}
	}, ModelObservationOptions{CompactPhases: true, SlowAfter: 10 * time.Millisecond})
	slow.emit(ctx, ModelCallStarted, "")
	slow.emit(ctx, ModelCallRequestWritten, "")
	select {
	case <-visible:
	case <-time.After(time.Second):
		t.Fatal("a slow call hid its arrived phase")
	}
	slow.finish(ctx, nil)
}
