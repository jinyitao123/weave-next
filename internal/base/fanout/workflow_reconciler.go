package fanout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type WorkflowReconcilerWorker struct {
	Transactions coordinatorTransactions
	Store        *Store
	Coordinator  FanoutCoordinator
	BatchSize    int
	PollInterval time.Duration
	Now          func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (w *WorkflowReconcilerWorker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func (w *WorkflowReconcilerWorker) batchSize() int {
	if w.BatchSize > 0 {
		return w.BatchSize
	}
	return 32
}

func (w *WorkflowReconcilerWorker) Sweep(ctx context.Context) (int, error) {
	if w == nil || w.Transactions == nil || w.Store == nil || w.Coordinator == nil {
		return 0, errors.New("workflow fanout reconciler dependencies are unavailable")
	}
	workspaces, err := w.listWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	processed := 0
	var sweepErrors []error
	for _, workspaceID := range workspaces {
		intents, voided, groups, err := w.listWorkspaceCandidates(ctx, workspaceID)
		if err != nil {
			sweepErrors = append(sweepErrors, fmt.Errorf("list fanout candidates for workspace %q: %w", workspaceID, err))
			continue
		}
		count, err := w.reconcileCandidates(ctx, workspaceID, append(intents, voided...), groups)
		processed += count
		if err != nil {
			sweepErrors = append(sweepErrors, err)
		}
		count, err = w.abandonExpiredCancellations(ctx, workspaceID)
		if err != nil {
			sweepErrors = append(sweepErrors, fmt.Errorf("reconcile fanout cancellations for workspace %q: %w", workspaceID, err))
			continue
		}
		processed += count
	}
	return processed, errors.Join(sweepErrors...)
}

func (w *WorkflowReconcilerWorker) reconcileCandidates(
	ctx context.Context,
	workspaceID string,
	intents []string,
	groups []string,
) (int, error) {
	processed := 0
	var candidateErrors []error
	for _, intentID := range intents {
		if err := w.Coordinator.ReconcileIntent(ctx, workspaceID, intentID); err != nil {
			candidateErrors = append(candidateErrors, fmt.Errorf("reconcile fanout intent %q: %w", intentID, err))
			continue
		}
		processed++
	}
	for _, groupID := range groups {
		if err := w.Coordinator.ReconcileGroup(ctx, workspaceID, groupID); err != nil {
			candidateErrors = append(candidateErrors, fmt.Errorf("reconcile fanout group %q: %w", groupID, err))
			continue
		}
		processed++
	}
	return processed, errors.Join(candidateErrors...)
}

func (w *WorkflowReconcilerWorker) listWorkspaces(ctx context.Context) ([]string, error) {
	tx, err := w.Transactions.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin fanout workspace scan: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	workspaces, err := w.Store.ListFanoutWorkspacesTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit fanout workspace scan: %w", err)
	}
	return workspaces, nil
}

func (w *WorkflowReconcilerWorker) listWorkspaceCandidates(ctx context.Context, workspaceID string) ([]string, []string, []string, error) {
	tx, err := w.Transactions.Begin(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin fanout candidate scan: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	pendingRows, err := w.Store.ListPendingIntentsForReconcileTx(ctx, tx, workspaceID, w.batchSize())
	if err != nil {
		return nil, nil, nil, err
	}
	voidedRows, err := w.Store.ListVoidedIntentsForReviveTx(ctx, tx, workspaceID, w.batchSize())
	if err != nil {
		return nil, nil, nil, err
	}
	groupRows, err := w.Store.ListGroupsForReconcileTx(ctx, tx, workspaceID, w.batchSize())
	if err != nil {
		return nil, nil, nil, err
	}
	pending := make([]string, 0, len(pendingRows))
	for _, intent := range pendingRows {
		pending = append(pending, intent.IntentID)
	}
	voided := make([]string, 0, len(voidedRows))
	for _, intent := range voidedRows {
		voided = append(voided, intent.IntentID)
	}
	groups := make([]string, 0, len(groupRows))
	for _, group := range groupRows {
		groups = append(groups, group.GroupID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("commit fanout candidate scan: %w", err)
	}
	return pending, voided, groups, nil
}

func (w *WorkflowReconcilerWorker) abandonExpiredCancellations(ctx context.Context, workspaceID string) (int, error) {
	tx, err := w.Transactions.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin fanout cancellation sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := w.now()
	legs, err := w.Store.ListExpiredLegCancellationsTx(ctx, tx, workspaceID, now, w.batchSize())
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, leg := range legs {
		won, err := w.Store.CASAbandonLegTx(ctx, tx, workspaceID, leg.GroupID, leg.LegID, leg.Generation, now)
		if err != nil {
			return 0, err
		}
		if won {
			changed++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit fanout cancellation sweep: %w", err)
	}
	return changed, nil
}

func (w *WorkflowReconcilerWorker) Start() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.cancel != nil {
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.mu.Unlock()
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			if _, err := w.Sweep(ctx); err != nil && ctx.Err() == nil {
				slog.Error("workflow fanout reconciler iteration failed", "error", err)
			}
			interval := w.PollInterval
			if interval <= 0 {
				interval = 2 * time.Second
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}()
}

func (w *WorkflowReconcilerWorker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	cancel := w.cancel
	w.cancel = nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		w.wg.Wait()
	}
}
