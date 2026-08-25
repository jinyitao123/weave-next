package loomruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type recoveryTerminalClaim struct {
	Lease   RunAttemptLease
	ClaimID uuid.UUID
}

type recoveryTerminalOutcome struct {
	Processed      bool
	Advanced       bool
	RetryScheduled bool
}

type TerminalReconcileHookStage string

const (
	TerminalReconcileHookStageExpiredLeaseClaimed   TerminalReconcileHookStage = "expired_lease_claimed"
	TerminalReconcileHookStageClaimCommitBefore     TerminalReconcileHookStage = "claim_transaction_commit_before"
	TerminalReconcileHookStageClaimCommitAfter      TerminalReconcileHookStage = "claim_transaction_commit_after"
	TerminalReconcileHookStageRecoveryRunLocked     TerminalReconcileHookStage = "recovery_run_locked"
	TerminalReconcileHookStageRecoveryFactsRead     TerminalReconcileHookStage = "recovery_facts_read"
	TerminalReconcileHookStageRecoveryAuditCompared TerminalReconcileHookStage = "recovery_audit_compared"
	TerminalReconcileHookStageRecoveryAuditWritten  TerminalReconcileHookStage = "recovery_audit_written"
	TerminalReconcileHookStageRecoveryMarkerWritten TerminalReconcileHookStage = "recovery_marker_written"
	TerminalReconcileHookStageRecoveryLeaseWritten  TerminalReconcileHookStage = "recovery_lease_written"
	TerminalReconcileHookStageRecoveryCommitBefore  TerminalReconcileHookStage = "recovery_transaction_commit_before"
	TerminalReconcileHookStageRecoveryCommitAfter   TerminalReconcileHookStage = "recovery_transaction_commit_after"
)

type terminalReconcileStageHookStore interface {
	TerminalReconcileStageHook(context.Context, TerminalReconcileHookStage) error
}

type TerminalReconcileStage string

const (
	TerminalReconcileValidate       TerminalReconcileStage = "validate"
	TerminalReconcileClaimBegin     TerminalReconcileStage = "claim_begin"
	TerminalReconcileClaimSelect    TerminalReconcileStage = "claim_select"
	TerminalReconcileClaimCommit    TerminalReconcileStage = "claim_commit"
	TerminalReconcileBegin          TerminalReconcileStage = "recovery_begin"
	TerminalReconcileLockRun        TerminalReconcileStage = "lock_run"
	TerminalReconcileLockRegistry   TerminalReconcileStage = "lock_registry"
	TerminalReconcileReadRegistry   TerminalReconcileStage = "read_registry"
	TerminalReconcileReadMarker     TerminalReconcileStage = "read_marker"
	TerminalReconcileReadLease      TerminalReconcileStage = "read_lease"
	TerminalReconcileReadClock      TerminalReconcileStage = "read_clock"
	TerminalReconcileReadCheckpoint TerminalReconcileStage = "read_checkpoint"
	TerminalReconcileLockAudit      TerminalReconcileStage = "lock_audit"
	TerminalReconcileReadAudit      TerminalReconcileStage = "read_audit"
	TerminalReconcileCompareAudit   TerminalReconcileStage = "compare_audit"
	TerminalReconcileWriteAudit     TerminalReconcileStage = "write_audit"
	TerminalReconcileWriteMarker    TerminalReconcileStage = "write_marker"
	TerminalReconcileWriteLease     TerminalReconcileStage = "write_lease"
	TerminalReconcileVerify         TerminalReconcileStage = "verify"
	TerminalReconcileCommit         TerminalReconcileStage = "commit"
	TerminalReconcileRecordRetry    TerminalReconcileStage = "record_retry"
)

type TerminalReconcileError struct {
	Stage       TerminalReconcileStage
	Code        string
	WorkspaceID string
	RunID       string
	Generation  int64
	AttemptID   uuid.UUID
	ClaimID     uuid.UUID
	Retryable   bool
	cause       error
}

func (err *TerminalReconcileError) Error() string {
	if err == nil {
		return "terminal reconcile failed"
	}
	return fmt.Sprintf(
		"terminal reconcile failed stage=%s code=%s workspace=%q run=%q generation=%d attempt=%q claim=%q retryable=%t",
		err.Stage,
		err.Code,
		err.WorkspaceID,
		err.RunID,
		err.Generation,
		err.AttemptID,
		err.ClaimID,
		err.Retryable,
	)
}

func (err *TerminalReconcileError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func newTerminalReconcileError(
	stage TerminalReconcileStage,
	code string,
	claim recoveryTerminalClaim,
	retryable bool,
	cause error,
) *TerminalReconcileError {
	return &TerminalReconcileError{
		Stage:       stage,
		Code:        code,
		WorkspaceID: claim.Lease.WorkspaceID,
		RunID:       claim.Lease.RunID,
		Generation:  claim.Lease.AttemptGeneration,
		AttemptID:   claim.Lease.AttemptID,
		ClaimID:     claim.ClaimID,
		Retryable:   retryable,
		cause:       cause,
	}
}

func runTerminalReconcileStageHook(
	ctx context.Context,
	store expectedRunAdmissionTxStore,
	stage TerminalReconcileHookStage,
) error {
	hooks, ok := store.(terminalReconcileStageHookStore)
	if !ok {
		return nil
	}
	if err := hooks.TerminalReconcileStageHook(ctx, stage); err != nil {
		return fmt.Errorf("terminal reconcile stage %s: %w", stage, err)
	}
	return nil
}

func recoveryCheckpointDefectRetryable(code string) bool {
	return code == recoveryCheckpointMissing
}

func preserveRecoveryMarkerLowerBound(
	candidate TerminalMarkerV1,
	current *TerminalMarkerV1,
) TerminalMarkerV1 {
	if current == nil ||
		candidate.AuditState != TerminalMarkerAuditBlocked {
		return candidate
	}
	candidate.UsageInputTokens = max(
		candidate.UsageInputTokens,
		current.UsageInputTokens,
	)
	candidate.UsageOutputTokens = max(
		candidate.UsageOutputTokens,
		current.UsageOutputTokens,
	)
	candidate.UsageCostUSD = math.Max(
		candidate.UsageCostUSD,
		current.UsageCostUSD,
	)
	candidate.CreatedAt = current.CreatedAt
	candidate.UpdatedAt = current.UpdatedAt
	return candidate
}

const (
	recoveryRegistryMissing  = "recovery_registry_missing"
	recoveryRegistryCorrupt  = "recovery_registry_corrupt"
	recoveryAuthorityInvalid = "recovery_authority_invalid"
	recoveryMarkerConflict   = "recovery_marker_conflict"
	recoveryClaimRetryTTL    = time.Minute
)

func commitClaimedRecoveryPG(
	ctx context.Context,
	store expectedRunAdmissionTxStore,
	claim recoveryTerminalClaim,
) (recoveryTerminalOutcome, error) {
	if err := validateRecoveryTerminalClaim(claim); err != nil {
		return recoveryTerminalOutcome{}, newTerminalReconcileError(
			TerminalReconcileValidate,
			"terminal_reconcile_validate",
			claim,
			false,
			err,
		)
	}
	if store == nil {
		return recoveryTerminalOutcome{}, newTerminalReconcileError(
			TerminalReconcileValidate,
			"terminal_reconcile_store_unavailable",
			claim,
			false,
			fmt.Errorf("recovery transaction store is unavailable"),
		)
	}
	fail := func(
		stage TerminalReconcileStage,
		code string,
		cause error,
	) (recoveryTerminalOutcome, error) {
		return recoveryTerminalOutcome{}, newTerminalReconcileError(
			stage,
			code,
			claim,
			true,
			cause,
		)
	}

	tx, err := store.BeginTx(ctx)
	if err != nil {
		return fail(
			TerminalReconcileBegin,
			"terminal_reconcile_begin",
			err,
		)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stateStore := NewPGTerminalStateStore()
	workspaceID := claim.Lease.WorkspaceID
	runID := claim.Lease.RunID
	if err := stateStore.LockTerminalRun(
		ctx,
		tx,
		workspaceID,
		runID,
	); err != nil {
		return fail(
			TerminalReconcileLockRun,
			"terminal_reconcile_lock_run",
			err,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryRunLocked,
	); err != nil {
		return fail(
			TerminalReconcileLockRun,
			"terminal_reconcile_hook_recovery_run_locked",
			err,
		)
	}

	registryNamespace := expectedRunNamespace(workspaceID)
	registryKey := expectedRunClaimKey(runID)
	if err := store.LockValueTx(
		ctx,
		tx,
		registryNamespace,
		registryKey,
	); err != nil {
		return fail(
			TerminalReconcileLockRegistry,
			"terminal_reconcile_lock_registry",
			err,
		)
	}
	registryBytes, registryPresent, err := store.ReadValueTx(
		ctx,
		tx,
		registryNamespace,
		registryKey,
	)
	if err != nil {
		return fail(
			TerminalReconcileReadRegistry,
			"terminal_reconcile_read_registry",
			err,
		)
	}

	currentMarker, markerPresent, markerReadErr :=
		stateStore.ReadTerminalMarkerForUpdate(
			ctx,
			tx,
			workspaceID,
			runID,
		)
	var markerDecodeErr *terminalMarkerDecodeError
	if markerReadErr != nil && !errors.As(markerReadErr, &markerDecodeErr) {
		return fail(
			TerminalReconcileReadMarker,
			"terminal_reconcile_read_marker",
			markerReadErr,
		)
	}

	currentLease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		workspaceID,
		runID,
	)
	if err != nil {
		return fail(
			TerminalReconcileReadLease,
			"terminal_reconcile_read_lease",
			err,
		)
	}
	if !leasePresent {
		return recoveryTerminalOutcome{}, nil
	}

	var dbNow time.Time
	if err := tx.QueryRow(
		ctx,
		"SELECT statement_timestamp()",
	).Scan(&dbNow); err != nil {
		return fail(
			TerminalReconcileReadClock,
			"terminal_reconcile_read_clock",
			err,
		)
	}
	if !recoveryClaimStillOwned(currentLease, claim, dbNow) {
		return recoveryTerminalOutcome{}, nil
	}

	checkpointBytes, checkpointPresent, err := store.ReadValueTx(
		ctx,
		tx,
		"checkpoint:"+currentLease.GraphName,
		runID,
	)
	if err != nil {
		return fail(
			TerminalReconcileReadCheckpoint,
			"terminal_reconcile_read_checkpoint",
			err,
		)
	}
	if !checkpointPresent {
		checkpointBytes = nil
	}

	auditNamespace := "audit:" + workspaceID
	if err := store.LockValueTx(
		ctx,
		tx,
		auditNamespace,
		runID,
	); err != nil {
		return fail(
			TerminalReconcileLockAudit,
			"terminal_reconcile_lock_audit",
			err,
		)
	}
	currentAudit, auditPresent, err := store.ReadValueTx(
		ctx,
		tx,
		auditNamespace,
		runID,
	)
	if err != nil {
		return fail(
			TerminalReconcileReadAudit,
			"terminal_reconcile_read_audit",
			err,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryFactsRead,
	); err != nil {
		return fail(
			TerminalReconcileReadAudit,
			"terminal_reconcile_hook_recovery_facts_read",
			err,
		)
	}

	registry, registryCode, registryErr := inspectRecoveryRegistry(
		registryBytes,
		registryPresent,
		workspaceID,
		runID,
		registryKey,
	)
	if registryErr != nil {
		return completeRecoveryLeaseOnly(
			ctx,
			tx,
			stateStore,
			store,
			claim,
			currentAudit,
			auditPresent,
			registryCode,
		)
	}
	if markerReadErr != nil {
		return completeRecoveryLeaseOnly(
			ctx,
			tx,
			stateStore,
			store,
			claim,
			currentAudit,
			auditPresent,
			recoveryMarkerConflict,
		)
	}

	inspection := InspectTerminalRecord(auditPresent, currentAudit)
	var normalRepairConflict bool
	if inspection.Classification == TerminalRecordValid &&
		inspection.Entry != nil &&
		isExistingNormalFinal(*inspection.Entry) {
		plan, err := prepareClaimedNormalFinalRepair(
			claim,
			registry,
			*inspection.Entry,
		)
		if err == nil {
			return applyClaimedNormalFinalAuditRepair(
				ctx,
				tx,
				store,
				stateStore,
				claim,
				currentMarkerPointer(currentMarker, markerPresent),
				currentAudit,
				plan,
			)
		}
		normalRepairConflict = true
	}

	assembly, err := AssembleRecoveryTerminalV3(
		RecoveryTerminalAssemblyInput{
			Registry:   registry,
			Lease:      currentLease,
			Checkpoint: checkpointBytes,
			RecordedAt: dbNow.UTC(),
		},
	)
	if err != nil {
		return completeRecoveryLeaseOnly(
			ctx,
			tx,
			stateStore,
			store,
			claim,
			currentAudit,
			auditPresent,
			recoveryAuthorityInvalid,
		)
	}
	if normalRepairConflict {
		return commitBlockedRecoveryAuditDefect(
			ctx,
			tx,
			store,
			stateStore,
			claim,
			currentMarkerPointer(currentMarker, markerPresent),
			currentAudit,
			auditPresent,
			assembly,
			recoveryAuditConflict,
		)
	}
	if inspection.Classification != TerminalRecordMissing &&
		inspection.Classification != TerminalRecordPreAttribution &&
		inspection.Classification != TerminalRecordValid {
		return commitBlockedRecoveryAuditDefect(
			ctx,
			tx,
			store,
			stateStore,
			claim,
			currentMarkerPointer(currentMarker, markerPresent),
			currentAudit,
			auditPresent,
			assembly,
			recoveryAuditCorrupt,
		)
	}
	if inspection.Classification == TerminalRecordValid &&
		inspection.Entry != nil &&
		inspection.Entry.Status == "failed" &&
		inspection.Entry.StopReason == "interrupted" &&
		assembly.Candidate == nil {
		return commitBlockedRecoveryAuditDefect(
			ctx,
			tx,
			store,
			stateStore,
			claim,
			currentMarkerPointer(currentMarker, markerPresent),
			currentAudit,
			auditPresent,
			assembly,
			recoveryAuditConflict,
		)
	}

	nextAudit := currentAudit
	nextAuditPresent := auditPresent
	if assembly.Candidate != nil {
		mutation, err := prepareTerminalMutation(*assembly.Candidate)
		if err != nil {
			return fail(
				TerminalReconcileCompareAudit,
				"terminal_reconcile_prepare_audit",
				err,
			)
		}
		nextAudit, err = mutation.applyPreservingCurrentLineage(
			currentAudit,
			auditPresent,
		)
		if errors.Is(err, ErrTerminalConflict) {
			return commitBlockedRecoveryAuditDefect(
				ctx,
				tx,
				store,
				stateStore,
				claim,
				currentMarkerPointer(currentMarker, markerPresent),
				currentAudit,
				auditPresent,
				assembly,
				recoveryAuditConflict,
			)
		}
		if err != nil {
			return fail(
				TerminalReconcileCompareAudit,
				"terminal_reconcile_compare_audit",
				err,
			)
		}
		nextAuditPresent = true
	}

	markerCandidate := preserveRecoveryMarkerLowerBound(
		assembly.Marker,
		currentMarkerPointer(currentMarker, markerPresent),
	)
	if err := ValidateTerminalMarkerTransition(
		currentMarkerPointer(currentMarker, markerPresent),
		markerCandidate,
	); err != nil {
		return completeRecoveryLeaseOnly(
			ctx,
			tx,
			stateStore,
			store,
			claim,
			currentAudit,
			auditPresent,
			recoveryMarkerConflict,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryAuditCompared,
	); err != nil {
		return fail(
			TerminalReconcileCompareAudit,
			"terminal_reconcile_hook_recovery_audit_compared",
			err,
		)
	}
	if nextAuditPresent &&
		(!auditPresent || !bytes.Equal(currentAudit, nextAudit)) {
		if err := store.PutValueTx(
			ctx,
			tx,
			auditNamespace,
			runID,
			nextAudit,
		); err != nil {
			return fail(
				TerminalReconcileWriteAudit,
				"terminal_reconcile_write_audit",
				err,
			)
		}
		if err := runTerminalReconcileStageHook(
			ctx,
			store,
			TerminalReconcileHookStageRecoveryAuditWritten,
		); err != nil {
			return fail(
				TerminalReconcileWriteAudit,
				"terminal_reconcile_hook_recovery_audit_written",
				err,
			)
		}
	}
	writtenMarker, err := stateStore.ApplyTerminalMarkerTransition(
		ctx,
		tx,
		markerCandidate,
	)
	if err != nil {
		return fail(
			TerminalReconcileWriteMarker,
			"terminal_reconcile_write_marker",
			err,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryMarkerWritten,
	); err != nil {
		return fail(
			TerminalReconcileWriteMarker,
			"terminal_reconcile_hook_recovery_marker_written",
			err,
		)
	}

	var writtenLease RunAttemptLease
	var retryScheduled bool
	if markerCandidate.LastErrorCode != nil &&
		recoveryCheckpointDefectRetryable(*markerCandidate.LastErrorCode) {
		writtenLease, _, retryScheduled, err =
			stateStore.retryClaimedAttemptLease(
				ctx,
				tx,
				attemptLeaseOwner(currentLease),
				claim.ClaimID,
				recoveryCheckpointMissing,
				recoveryClaimRetryTTL,
			)
		if err != nil {
			return fail(
				TerminalReconcileWriteLease,
				"terminal_reconcile_retry_lease",
				err,
			)
		}
		if !retryScheduled {
			return recoveryTerminalOutcome{}, nil
		}
	} else {
		var completed bool
		var code *string
		if markerCandidate.LastErrorCode != nil {
			value := *markerCandidate.LastErrorCode
			code = &value
		}
		writtenLease, completed, err =
			stateStore.completeClaimedAttemptLease(
				ctx,
				tx,
				attemptLeaseOwner(currentLease),
				claim.ClaimID,
				AttemptLeaseReconciled,
				code,
			)
		if err != nil {
			return fail(
				TerminalReconcileWriteLease,
				"terminal_reconcile_complete_lease",
				err,
			)
		}
		if !completed {
			return recoveryTerminalOutcome{}, nil
		}
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryLeaseWritten,
	); err != nil {
		return fail(
			TerminalReconcileWriteLease,
			"terminal_reconcile_hook_recovery_lease_written",
			err,
		)
	}

	if err := verifyRecoveryWrites(
		ctx,
		tx,
		store,
		stateStore,
		claim,
		nextAudit,
		nextAuditPresent,
		writtenMarker,
		writtenLease,
	); err != nil {
		return fail(
			TerminalReconcileVerify,
			"terminal_reconcile_verify",
			err,
		)
	}
	if err := commitRecoveryTransaction(
		ctx,
		tx,
		store,
		claim,
		"terminal_reconcile_commit",
	); err != nil {
		return recoveryTerminalOutcome{}, err
	}
	return recoveryTerminalOutcome{
		Processed:      true,
		Advanced:       true,
		RetryScheduled: retryScheduled,
	}, nil
}

func validateRecoveryTerminalClaim(claim recoveryTerminalClaim) error {
	if err := ValidateRunAttemptLease(claim.Lease); err != nil {
		return fmt.Errorf("validate claimed lease: %w", err)
	}
	if claim.Lease.State != AttemptLeaseReconciling {
		return fmt.Errorf("claimed lease state must be reconciling")
	}
	if claim.ClaimID == uuid.Nil {
		return fmt.Errorf("claim_id must be non-zero")
	}
	if claim.Lease.ClaimID == nil || *claim.Lease.ClaimID != claim.ClaimID {
		return fmt.Errorf("claim snapshot does not own claim_id")
	}
	return nil
}

func recoveryClaimStillOwned(
	current RunAttemptLease,
	claim recoveryTerminalClaim,
	dbNow time.Time,
) bool {
	return attemptLeaseOwnerEqual(current, attemptLeaseOwner(claim.Lease)) &&
		current.State == AttemptLeaseReconciling &&
		current.ClaimID != nil &&
		*current.ClaimID == claim.ClaimID &&
		current.ClaimExpiresAt != nil &&
		current.ClaimExpiresAt.After(dbNow)
}

func inspectRecoveryRegistry(
	data []byte,
	present bool,
	workspaceID string,
	runID string,
	key string,
) (ExpectedRunRecordV1, string, error) {
	if !present {
		return ExpectedRunRecordV1{}, recoveryRegistryMissing,
			fmt.Errorf("expected run registry is missing")
	}
	record, err := inspectExpectedRunPhysicalRecord(
		data,
		workspaceID,
		runID,
		key,
		true,
	)
	if err != nil {
		return ExpectedRunRecordV1{}, recoveryRegistryCorrupt, err
	}
	return record, "", nil
}

func currentMarkerPointer(
	marker TerminalMarkerV1,
	present bool,
) *TerminalMarkerV1 {
	if !present {
		return nil
	}
	return &marker
}

func isExistingNormalFinal(entry TerminalEntryV3) bool {
	return entry.Status == "success" ||
		(entry.Status == "failed" && entry.StopReason != "interrupted")
}

func prepareClaimedNormalFinalRepair(
	claim recoveryTerminalClaim,
	registry ExpectedRunRecordV1,
	entry TerminalEntryV3,
) (normalTerminalCommitPlan, error) {
	plan, err := prepareNormalTerminalCommit(NormalTerminalCommit{
		Candidate: entry,
		Owner:     attemptLeaseOwner(claim.Lease),
	})
	if err == nil {
		err = compareExpectedRunIdentity(plan.Claim, registry)
	}
	return plan, err
}

func applyClaimedNormalFinalAuditRepair(
	ctx context.Context,
	tx pgx.Tx,
	store expectedRunAdmissionTxStore,
	stateStore *PGTerminalStateStore,
	claim recoveryTerminalClaim,
	currentMarker *TerminalMarkerV1,
	currentAudit []byte,
	plan normalTerminalCommitPlan,
) (recoveryTerminalOutcome, error) {
	markerCandidate := plan.Marker
	if currentMarker != nil {
		markerCandidate.CreatedAt = currentMarker.CreatedAt
		markerCandidate.UpdatedAt = currentMarker.UpdatedAt
		if currentMarker.TerminalAt.Equal(
			markerCandidate.TerminalAt.Truncate(time.Microsecond),
		) {
			markerCandidate.TerminalAt = currentMarker.TerminalAt
		}
		if markerLineageBusinessEqual(*currentMarker, markerCandidate) {
			markerCandidate.LineageState = currentMarker.LineageState
			markerCandidate.LastErrorCode = currentMarker.LastErrorCode
		}
	}
	if err := ValidateTerminalMarkerTransition(
		currentMarker,
		markerCandidate,
	); err != nil {
		return completeRecoveryLeaseOnly(
			ctx,
			tx,
			stateStore,
			store,
			claim,
			currentAudit,
			true,
			recoveryMarkerConflict,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryAuditCompared,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileCompareAudit,
			"terminal_reconcile_hook_recovery_audit_compared",
			claim,
			err,
		)
	}
	writtenMarker, err := stateStore.ApplyTerminalMarkerTransition(
		ctx,
		tx,
		markerCandidate,
	)
	if err != nil {
		return recoveryFailure(
			TerminalReconcileWriteMarker,
			"terminal_reconcile_write_normal_marker",
			claim,
			err,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryMarkerWritten,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileWriteMarker,
			"terminal_reconcile_hook_recovery_marker_written",
			claim,
			err,
		)
	}
	writtenLease, completed, err :=
		stateStore.completeClaimedAttemptLease(
			ctx,
			tx,
			attemptLeaseOwner(claim.Lease),
			claim.ClaimID,
			AttemptLeaseClosed,
			nil,
		)
	if err != nil {
		return recoveryFailure(
			TerminalReconcileWriteLease,
			"terminal_reconcile_close_normal_lease",
			claim,
			err,
		)
	}
	if !completed {
		return recoveryTerminalOutcome{}, nil
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryLeaseWritten,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileWriteLease,
			"terminal_reconcile_hook_recovery_lease_written",
			claim,
			err,
		)
	}
	if err := verifyRecoveryWrites(
		ctx,
		tx,
		store,
		stateStore,
		claim,
		currentAudit,
		true,
		writtenMarker,
		writtenLease,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileVerify,
			"terminal_reconcile_verify_normal_repair",
			claim,
			err,
		)
	}
	if err := commitRecoveryTransaction(
		ctx,
		tx,
		store,
		claim,
		"terminal_reconcile_commit_normal_repair",
	); err != nil {
		return recoveryTerminalOutcome{}, err
	}
	return recoveryTerminalOutcome{Processed: true, Advanced: true}, nil
}

func commitBlockedRecoveryAuditDefect(
	ctx context.Context,
	tx pgx.Tx,
	store expectedRunAdmissionTxStore,
	stateStore *PGTerminalStateStore,
	claim recoveryTerminalClaim,
	currentMarker *TerminalMarkerV1,
	currentAudit []byte,
	auditPresent bool,
	assembly RecoveryTerminalAssembly,
	code string,
) (recoveryTerminalOutcome, error) {
	marker := assembly.Marker
	marker.EvidenceKind = TerminalMarkerEvidenceRegistryOnly
	marker.CheckpointGraph = nil
	marker.CheckpointSeq = nil
	marker.CheckpointSavedAt = nil
	marker.AuditState = TerminalMarkerAuditBlocked
	marker.AuditSchemaVersion = nil
	marker.LastErrorCode = &code
	marker = preserveRecoveryMarkerLowerBound(marker, currentMarker)
	if err := ValidateTerminalMarkerTransition(currentMarker, marker); err != nil {
		return completeRecoveryLeaseOnly(
			ctx,
			tx,
			stateStore,
			store,
			claim,
			currentAudit,
			auditPresent,
			recoveryMarkerConflict,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryAuditCompared,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileCompareAudit,
			"terminal_reconcile_hook_recovery_audit_compared",
			claim,
			err,
		)
	}
	writtenMarker, err := stateStore.ApplyTerminalMarkerTransition(
		ctx,
		tx,
		marker,
	)
	if err != nil {
		return recoveryFailure(
			TerminalReconcileWriteMarker,
			"terminal_reconcile_write_blocked_marker",
			claim,
			err,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryMarkerWritten,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileWriteMarker,
			"terminal_reconcile_hook_recovery_marker_written",
			claim,
			err,
		)
	}
	writtenLease, completed, err :=
		stateStore.completeClaimedAttemptLease(
			ctx,
			tx,
			attemptLeaseOwner(claim.Lease),
			claim.ClaimID,
			AttemptLeaseReconciled,
			&code,
		)
	if err != nil {
		return recoveryFailure(
			TerminalReconcileWriteLease,
			"terminal_reconcile_complete_blocked_lease",
			claim,
			err,
		)
	}
	if !completed {
		return recoveryTerminalOutcome{}, nil
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryLeaseWritten,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileWriteLease,
			"terminal_reconcile_hook_recovery_lease_written",
			claim,
			err,
		)
	}
	if err := verifyRecoveryWrites(
		ctx,
		tx,
		store,
		stateStore,
		claim,
		currentAudit,
		auditPresent,
		writtenMarker,
		writtenLease,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileVerify,
			"terminal_reconcile_verify_blocked",
			claim,
			err,
		)
	}
	if err := commitRecoveryTransaction(
		ctx,
		tx,
		store,
		claim,
		"terminal_reconcile_commit_blocked",
	); err != nil {
		return recoveryTerminalOutcome{}, err
	}
	return recoveryTerminalOutcome{Processed: true, Advanced: true}, nil
}

func completeRecoveryLeaseOnly(
	ctx context.Context,
	tx pgx.Tx,
	stateStore *PGTerminalStateStore,
	store expectedRunAdmissionTxStore,
	claim recoveryTerminalClaim,
	currentAudit []byte,
	auditPresent bool,
	code string,
) (recoveryTerminalOutcome, error) {
	writtenLease, completed, err :=
		stateStore.completeClaimedAttemptLease(
			ctx,
			tx,
			attemptLeaseOwner(claim.Lease),
			claim.ClaimID,
			AttemptLeaseReconciled,
			&code,
		)
	if err != nil {
		return recoveryFailure(
			TerminalReconcileWriteLease,
			"terminal_reconcile_complete_defect_lease",
			claim,
			err,
		)
	}
	if !completed {
		return recoveryTerminalOutcome{}, nil
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryLeaseWritten,
	); err != nil {
		return recoveryFailure(
			TerminalReconcileWriteLease,
			"terminal_reconcile_hook_recovery_lease_written",
			claim,
			err,
		)
	}
	verifiedAudit, verifiedAuditPresent, err := store.ReadValueTx(
		ctx,
		tx,
		"audit:"+claim.Lease.WorkspaceID,
		claim.Lease.RunID,
	)
	if err != nil {
		return recoveryFailure(
			TerminalReconcileVerify,
			"terminal_reconcile_verify_defect_audit",
			claim,
			err,
		)
	}
	verifiedLease, leasePresent, err :=
		stateStore.ReadAttemptLeaseForUpdate(
			ctx,
			tx,
			claim.Lease.WorkspaceID,
			claim.Lease.RunID,
		)
	if err != nil ||
		!leasePresent ||
		verifiedAuditPresent != auditPresent ||
		!bytes.Equal(verifiedAudit, currentAudit) ||
		!reflect.DeepEqual(verifiedLease, writtenLease) {
		if err == nil {
			err = fmt.Errorf("lease-only recovery durable verification mismatch")
		}
		return recoveryFailure(
			TerminalReconcileVerify,
			"terminal_reconcile_verify_defect",
			claim,
			err,
		)
	}
	if err := commitRecoveryTransaction(
		ctx,
		tx,
		store,
		claim,
		"terminal_reconcile_commit_defect",
	); err != nil {
		return recoveryTerminalOutcome{}, err
	}
	return recoveryTerminalOutcome{Processed: true, Advanced: true}, nil
}

func verifyRecoveryWrites(
	ctx context.Context,
	tx pgx.Tx,
	store expectedRunAdmissionTxStore,
	stateStore *PGTerminalStateStore,
	claim recoveryTerminalClaim,
	wantAudit []byte,
	wantAuditPresent bool,
	wantMarker TerminalMarkerV1,
	wantLease RunAttemptLease,
) error {
	audit, auditPresent, err := store.ReadValueTx(
		ctx,
		tx,
		"audit:"+claim.Lease.WorkspaceID,
		claim.Lease.RunID,
	)
	if err != nil {
		return err
	}
	marker, markerPresent, err := stateStore.ReadTerminalMarkerForUpdate(
		ctx,
		tx,
		claim.Lease.WorkspaceID,
		claim.Lease.RunID,
	)
	if err != nil {
		return err
	}
	lease, leasePresent, err := stateStore.ReadAttemptLeaseForUpdate(
		ctx,
		tx,
		claim.Lease.WorkspaceID,
		claim.Lease.RunID,
	)
	if err != nil {
		return err
	}
	if auditPresent != wantAuditPresent ||
		!bytes.Equal(audit, wantAudit) ||
		!markerPresent ||
		!reflect.DeepEqual(marker, wantMarker) ||
		!leasePresent ||
		!reflect.DeepEqual(lease, wantLease) {
		return fmt.Errorf("recovery durable verification mismatch")
	}
	return nil
}

func commitRecoveryTransaction(
	ctx context.Context,
	tx pgx.Tx,
	store expectedRunAdmissionTxStore,
	claim recoveryTerminalClaim,
	commitCode string,
) error {
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryCommitBefore,
	); err != nil {
		return newTerminalReconcileError(
			TerminalReconcileCommit,
			"terminal_reconcile_hook_recovery_commit_before",
			claim,
			true,
			err,
		)
	}
	if err := tx.Commit(ctx); err != nil {
		return newTerminalReconcileError(
			TerminalReconcileCommit,
			commitCode,
			claim,
			true,
			err,
		)
	}
	if err := runTerminalReconcileStageHook(
		ctx,
		store,
		TerminalReconcileHookStageRecoveryCommitAfter,
	); err != nil {
		return newTerminalReconcileError(
			TerminalReconcileCommit,
			"terminal_reconcile_hook_recovery_commit_after",
			claim,
			true,
			err,
		)
	}
	return nil
}

func recoveryFailure(
	stage TerminalReconcileStage,
	code string,
	claim recoveryTerminalClaim,
	cause error,
) (recoveryTerminalOutcome, error) {
	return recoveryTerminalOutcome{}, newTerminalReconcileError(
		stage,
		code,
		claim,
		true,
		cause,
	)
}
