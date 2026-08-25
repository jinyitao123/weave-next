package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrA4TerminalReconcilerUnsupported = errors.New(
	"terminal reconciler requires PostgreSQL transaction support",
)

const terminalReconcilerClaimTTL = time.Minute

const terminalReconcilerRetryFallbackCode = "terminal_reconcile_retryable"

type TerminalReconciler interface {
	ReconcileOne(context.Context) (bool, error)
}

type terminalReconciler struct {
	records TerminalRecordStore
	store   expectedRunAdmissionTxStore
	turn    atomic.Uint64
}

func NewTerminalReconciler(
	store TerminalRecordStore,
) (TerminalReconciler, error) {
	if store == nil || isNilTerminalRecordStore(store) {
		return nil, ErrA4TerminalReconcilerUnsupported
	}
	txStore, ok := store.(expectedRunAdmissionTxStore)
	if !ok {
		return nil, fmt.Errorf(
			"%w: store has no transaction extension",
			ErrA4TerminalReconcilerUnsupported,
		)
	}
	return &terminalReconciler{
		records: store,
		store:   txStore,
	}, nil
}

func (reconciler *terminalReconciler) ReconcileOne(
	ctx context.Context,
) (bool, error) {
	claim, ok, err := reconciler.claimOne(ctx, uuid.New())
	if err != nil || !ok {
		return false, err
	}
	outcome, err := commitClaimedRecoveryPG(ctx, reconciler.store, claim)
	if err != nil {
		_ = reconciler.recordRetryableFailure(ctx, claim, err)
		return true, err
	}
	if !outcome.Processed {
		return true, nil
	}
	_, _, err = repairTerminalLineageRun(
		ctx,
		reconciler.records,
		claim.Lease.WorkspaceID,
		claim.Lease.RunID,
	)
	return true, err
}

func (reconciler *terminalReconciler) claimOne(
	ctx context.Context,
	claimID uuid.UUID,
) (recoveryTerminalClaim, bool, error) {
	claim := recoveryTerminalClaim{ClaimID: claimID}
	tx, err := reconciler.store.BeginTx(ctx)
	if err != nil {
		return recoveryTerminalClaim{}, false, newTerminalReconcileError(
			TerminalReconcileClaimBegin,
			"terminal_reconcile_claim_begin",
			claim,
			true,
			err,
		)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	stateStore := NewPGTerminalStateStore()
	type claimOperation func(
		context.Context,
		pgx.Tx,
		uuid.UUID,
		time.Duration,
	) (RunAttemptLease, time.Time, bool, error)
	fresh := claimOperation(stateStore.ClaimExpiredAttemptLease)
	retry := claimOperation(stateStore.ReclaimExpiredAttemptLease)
	operations := [2]claimOperation{fresh, retry}
	if reconciler.turn.Add(1)&1 == 0 {
		operations = [2]claimOperation{retry, fresh}
	}

	for _, operation := range operations {
		lease, _, ok, err := operation(
			ctx,
			tx,
			claimID,
			terminalReconcilerClaimTTL,
		)
		if err != nil {
			return recoveryTerminalClaim{}, false, newTerminalReconcileError(
				TerminalReconcileClaimSelect,
				"terminal_reconcile_claim_select",
				claim,
				true,
				err,
			)
		}
		if !ok {
			continue
		}
		claim.Lease = lease
		if err := runTerminalReconcileStageHook(
			ctx,
			reconciler.store,
			TerminalReconcileHookStageExpiredLeaseClaimed,
		); err != nil {
			return recoveryTerminalClaim{}, false, newTerminalReconcileError(
				TerminalReconcileClaimSelect,
				"terminal_reconcile_hook_expired_lease_claimed",
				claim,
				true,
				err,
			)
		}
		if err := runTerminalReconcileStageHook(
			ctx,
			reconciler.store,
			TerminalReconcileHookStageClaimCommitBefore,
		); err != nil {
			return recoveryTerminalClaim{}, false, newTerminalReconcileError(
				TerminalReconcileClaimCommit,
				"terminal_reconcile_hook_claim_commit_before",
				claim,
				true,
				err,
			)
		}
		if err := tx.Commit(ctx); err != nil {
			return recoveryTerminalClaim{}, false, newTerminalReconcileError(
				TerminalReconcileClaimCommit,
				"terminal_reconcile_claim_commit",
				claim,
				true,
				err,
			)
		}
		finished = true
		if err := runTerminalReconcileStageHook(
			ctx,
			reconciler.store,
			TerminalReconcileHookStageClaimCommitAfter,
		); err != nil {
			return recoveryTerminalClaim{}, false, newTerminalReconcileError(
				TerminalReconcileClaimCommit,
				"terminal_reconcile_hook_claim_commit_after",
				claim,
				true,
				err,
			)
		}
		return claim, true, nil
	}
	_ = tx.Rollback(ctx)
	finished = true
	return recoveryTerminalClaim{}, false, nil
}

func (reconciler *terminalReconciler) recordRetryableFailure(
	ctx context.Context,
	claim recoveryTerminalClaim,
	sourceErr error,
) error {
	code := terminalReconcilerRetryFallbackCode
	var reconcileErr *TerminalReconcileError
	if errors.As(sourceErr, &reconcileErr) && reconcileErr.Code != "" {
		code = reconcileErr.Code
	}

	fail := func(stableCode string, cause error) error {
		return newTerminalReconcileError(
			TerminalReconcileRecordRetry,
			stableCode,
			claim,
			true,
			cause,
		)
	}
	tx, err := reconciler.store.BeginTx(ctx)
	if err != nil {
		return fail("terminal_reconcile_record_retry_begin", err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback(ctx)
		}
	}()

	_, _, recorded, err := NewPGTerminalStateStore().
		retryClaimedAttemptLease(
			ctx,
			tx,
			attemptLeaseOwner(claim.Lease),
			claim.ClaimID,
			code,
			terminalReconcilerClaimTTL,
		)
	if err != nil {
		return fail("terminal_reconcile_record_retry_write", err)
	}
	if !recorded {
		_ = tx.Rollback(ctx)
		finished = true
		return nil
	}
	if err := tx.Commit(ctx); err != nil {
		return fail("terminal_reconcile_record_retry_commit", err)
	}
	finished = true
	return nil
}
