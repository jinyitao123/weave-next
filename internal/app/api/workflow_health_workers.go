package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/workflowhealth"
)

const workflowHealthPollInterval = 30 * time.Second

type workflowHealthWorkers struct {
	store  *workflowhealth.Store
	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (workers *workflowHealthWorkers) Start() {
	if workers == nil || workers.store == nil {
		return
	}
	workers.mu.Lock()
	if workers.cancel != nil {
		workers.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	workers.cancel = cancel
	workers.mu.Unlock()
	workers.wg.Add(1)
	go func() {
		defer workers.wg.Done()
		for {
			if _, err := workers.store.ObserveBatch(ctx, 32); err != nil && ctx.Err() == nil {
				slog.Error("workflow health observation failed", "error", err)
			}
			timer := time.NewTimer(workflowHealthPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (workers *workflowHealthWorkers) Stop() {
	if workers == nil {
		return
	}
	workers.mu.Lock()
	cancel := workers.cancel
	workers.cancel = nil
	workers.mu.Unlock()
	if cancel != nil {
		cancel()
		workers.wg.Wait()
	}
}
