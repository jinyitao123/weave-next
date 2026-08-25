package loomruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrFrozenAttemptAdmissionUnsupported = errors.New("frozen attempt admission is unsupported")
	ErrFrozenAttemptSuperseded           = errors.New("frozen attempt generation is superseded")
)

// FrozenAttemptAdmission binds one already-claimed frozen execution epoch to
// the runtime registry and attempt domain without constructing a PreparedRun.
type FrozenAttemptAdmission struct {
	Record            ExpectedRunRecordV1
	AttemptGeneration int64
	AttemptID         uuid.UUID
	GraphName         string
	RunStartedAt      string
	LeaseTTL          time.Duration
}

type FrozenAttemptAdvance struct {
	Current        AttemptLeaseOwner
	NextGeneration int64
	NextAttemptID  uuid.UUID
	LeaseTTL       time.Duration
}

// AdvanceFrozenAttemptTx fences one exact active owner and installs the next
// execution epoch in the caller's TeamRun transition transaction.
func AdvanceFrozenAttemptTx(
	ctx context.Context,
	tx pgx.Tx,
	request FrozenAttemptAdvance,
) (RunAttemptLease, bool, error) {
	if tx == nil {
		return RunAttemptLease{}, false, fmt.Errorf("tx must be non-nil")
	}
	if err := validateAttemptLeaseOwner(request.Current); err != nil {
		return RunAttemptLease{}, false, fmt.Errorf("validate current frozen attempt: %w", err)
	}
	if request.Current.Generation == int64(^uint64(0)>>1) ||
		request.NextGeneration != request.Current.Generation+1 {
		return RunAttemptLease{}, false, fmt.Errorf("next attempt_generation must increment current generation")
	}
	if request.NextAttemptID == uuid.Nil || request.NextAttemptID == request.Current.AttemptID {
		return RunAttemptLease{}, false, fmt.Errorf("next attempt_id must be non-zero and differ from current")
	}
	if request.LeaseTTL <= 0 {
		return RunAttemptLease{}, false, fmt.Errorf("lease_ttl must be positive")
	}
	query := `WITH db_clock AS MATERIALIZED (
			SELECT statement_timestamp() AS db_now
		)
		UPDATE weave_run_attempt_leases AS lease
		SET attempt_generation=$7,
			attempt_id=$8,
			attempt_started_at=db_clock.db_now,
			state='active',
			heartbeat_at=db_clock.db_now,
			lease_expires_at=db_clock.db_now+$9::interval,
			claim_id=NULL,
			claim_expires_at=NULL,
			last_error_code=NULL,
			retry_count=0
		FROM db_clock
		WHERE lease.workspace_id=$1
			AND lease.run_id=$2
			AND lease.attempt_generation=$3
			AND lease.attempt_id=$4
			AND lease.graph_name=$5
			AND lease.run_started_at=$6
			AND lease.state='active'
		RETURNING ` + attemptLeaseColumns
	lease, err := scanAttemptLease(tx.QueryRow(
		ctx,
		query,
		request.Current.WorkspaceID,
		request.Current.RunID,
		request.Current.Generation,
		request.Current.AttemptID,
		request.Current.GraphName,
		request.Current.RunStartedAt,
		request.NextGeneration,
		request.NextAttemptID,
		request.LeaseTTL.String(),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunAttemptLease{}, false, nil
	}
	if err != nil {
		return RunAttemptLease{}, false, fmt.Errorf("advance frozen attempt lease: %w", err)
	}
	return lease, true, nil
}

// AdmitFrozenAttempt atomically claims the expected run identity and creates
// the active attempt owner for an already-committed TeamRun execution epoch.
func AdmitFrozenAttempt(
	ctx context.Context,
	store TerminalRecordStore,
	request FrozenAttemptAdmission,
) (RunAttemptLease, error) {
	implementation, _, err := prepareFrozenAttemptAdmission(store, request)
	if err != nil {
		return RunAttemptLease{}, err
	}

	tx, err := implementation.txStore.BeginTx(ctx)
	if err != nil {
		return RunAttemptLease{}, fmt.Errorf("begin frozen attempt admission: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lease, err := AdmitFrozenAttemptTx(ctx, store, tx, request)
	if err != nil {
		return RunAttemptLease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RunAttemptLease{}, fmt.Errorf("commit frozen attempt admission: %w", err)
	}
	return lease, nil
}

// AdmitFrozenAttemptTx performs frozen admission in the caller's transaction.
// The caller owns commit and rollback.
func AdmitFrozenAttemptTx(
	ctx context.Context,
	store TerminalRecordStore,
	tx pgx.Tx,
	request FrozenAttemptAdmission,
) (RunAttemptLease, error) {
	if tx == nil {
		return RunAttemptLease{}, fmt.Errorf("tx must be non-nil")
	}
	implementation, candidateBytes, err := prepareFrozenAttemptAdmission(store, request)
	if err != nil {
		return RunAttemptLease{}, err
	}
	stateStore := NewPGTerminalStateStore()
	if err := stateStore.LockTerminalRun(
		ctx, tx, request.Record.WorkspaceID, request.Record.RunID,
	); err != nil {
		return RunAttemptLease{}, fmt.Errorf("lock frozen attempt run: %w", err)
	}
	namespace := expectedRunNamespace(request.Record.WorkspaceID)
	claimKey := expectedRunClaimKey(request.Record.RunID)
	indexKey := expectedRunDomainKey(request.Record)
	for _, key := range []string{claimKey, indexKey} {
		if err := implementation.txStore.LockValueTx(ctx, tx, namespace, key); err != nil {
			return RunAttemptLease{}, fmt.Errorf("lock frozen expected run %q: %w", key, err)
		}
	}

	_, err = reconcileFrozenExpectedRun(
		ctx,
		implementation.txStore,
		tx,
		request.Record,
		candidateBytes,
		namespace,
		claimKey,
		indexKey,
	)
	if err != nil {
		return RunAttemptLease{}, err
	}
	lease, err := createFrozenAttemptLease(ctx, tx, request)
	if err != nil {
		return RunAttemptLease{}, err
	}
	verifiedClaim, claimPresent, err := implementation.txStore.ReadValueTx(
		ctx, tx, namespace, claimKey,
	)
	if err != nil {
		return RunAttemptLease{}, fmt.Errorf("verify frozen expected run claim: %w", err)
	}
	verifiedIndex, indexPresent, err := implementation.txStore.ReadValueTx(
		ctx, tx, namespace, indexKey,
	)
	if err != nil {
		return RunAttemptLease{}, fmt.Errorf("verify frozen expected run index: %w", err)
	}
	if err := verifyFrozenExpectedRun(
		request.Record,
		verifiedClaim,
		claimPresent,
		verifiedIndex,
		indexPresent,
		claimKey,
		indexKey,
	); err != nil {
		return RunAttemptLease{}, err
	}
	return lease, nil
}

func verifyFrozenExpectedRun(
	candidate ExpectedRunRecordV1,
	claimBytes []byte,
	claimPresent bool,
	indexBytes []byte,
	indexPresent bool,
	claimKey string,
	indexKey string,
) error {
	if !claimPresent || !indexPresent {
		return fmt.Errorf("verify frozen expected run presence")
	}
	claim, err := inspectExpectedRunPhysicalRecord(
		claimBytes, candidate.WorkspaceID, candidate.RunID, claimKey, true,
	)
	if err != nil {
		return fmt.Errorf("inspect verified frozen expected run claim: %w", err)
	}
	indexed, err := inspectExpectedRunPhysicalRecord(
		indexBytes, candidate.WorkspaceID, candidate.RunID, indexKey, false,
	)
	if err != nil {
		return fmt.Errorf("inspect verified frozen expected run index: %w", err)
	}
	if err := compareExpectedRunIdentity(claim, candidate); err != nil {
		return fmt.Errorf("verify frozen expected run claim identity: %w", err)
	}
	if err := compareExpectedRunIdentity(indexed, candidate); err != nil {
		return fmt.Errorf("verify frozen expected run index identity: %w", err)
	}
	canonicalClaim, err := encodeExpectedRunRecordV1(claim)
	if err != nil {
		return fmt.Errorf("encode verified frozen expected run claim: %w", err)
	}
	canonicalIndex, err := encodeExpectedRunRecordV1(indexed)
	if err != nil {
		return fmt.Errorf("encode verified frozen expected run index: %w", err)
	}
	if !bytes.Equal(claimBytes, canonicalClaim) ||
		!bytes.Equal(indexBytes, canonicalIndex) ||
		!bytes.Equal(claimBytes, indexBytes) {
		return fmt.Errorf("verify frozen expected run canonical bytes")
	}
	return nil
}

func prepareFrozenAttemptAdmission(
	store TerminalRecordStore,
	request FrozenAttemptAdmission,
) (*expectedRunRegistry, []byte, error) {
	if store == nil || isNilTerminalRecordStore(store) {
		return nil, nil, ErrFrozenAttemptAdmissionUnsupported
	}
	registry, err := NewExpectedRunRegistry(store)
	if err != nil {
		return nil, nil, err
	}
	implementation := registry.(*expectedRunRegistry)
	if implementation.txStore == nil {
		return nil, nil, ErrFrozenAttemptAdmissionUnsupported
	}
	candidateBytes, err := encodeExpectedRunRecordV1(request.Record)
	if err != nil {
		return nil, nil, fmt.Errorf("validate frozen expected run: %w", err)
	}
	if request.AttemptGeneration < 1 {
		return nil, nil, fmt.Errorf("attempt_generation must be >= 1")
	}
	if request.AttemptID == uuid.Nil {
		return nil, nil, fmt.Errorf("attempt_id must be non-zero")
	}
	if request.GraphName == "" {
		return nil, nil, fmt.Errorf("graph_name must be non-empty")
	}
	if err := canonicalRunStartedAt(request.RunStartedAt); err != nil {
		return nil, nil, err
	}
	if request.LeaseTTL <= 0 {
		return nil, nil, fmt.Errorf("lease_ttl must be positive")
	}
	return implementation, candidateBytes, nil
}

func reconcileFrozenExpectedRun(
	ctx context.Context,
	store expectedRunAdmissionTxStore,
	tx pgx.Tx,
	candidate ExpectedRunRecordV1,
	candidateBytes []byte,
	namespace string,
	claimKey string,
	indexKey string,
) ([]byte, error) {
	claimBytes, claimPresent, err := store.ReadValueTx(ctx, tx, namespace, claimKey)
	if err != nil {
		return nil, fmt.Errorf("read frozen expected run claim: %w", err)
	}
	indexBytes, indexPresent, err := store.ReadValueTx(ctx, tx, namespace, indexKey)
	if err != nil {
		return nil, fmt.Errorf("read frozen expected run index: %w", err)
	}

	canonicalBytes := candidateBytes
	if claimPresent {
		claim, err := inspectExpectedRunPhysicalRecord(
			claimBytes, candidate.WorkspaceID, candidate.RunID, claimKey, true,
		)
		if err != nil {
			return nil, fmt.Errorf("inspect frozen expected run claim: %w", err)
		}
		if err := compareExpectedRunIdentity(claim, candidate); err != nil {
			return nil, err
		}
		canonicalBytes = claimBytes
	}
	if indexPresent {
		indexed, err := inspectExpectedRunPhysicalRecord(
			indexBytes, candidate.WorkspaceID, candidate.RunID, indexKey, false,
		)
		if err != nil {
			return nil, fmt.Errorf("inspect frozen expected run index: %w", err)
		}
		if err := compareExpectedRunIdentity(indexed, candidate); err != nil {
			return nil, err
		}
		if !claimPresent {
			canonicalBytes = indexBytes
		}
	}
	if !claimPresent || !bytes.Equal(claimBytes, canonicalBytes) {
		if err := store.PutValueTx(ctx, tx, namespace, claimKey, canonicalBytes); err != nil {
			return nil, fmt.Errorf("write frozen expected run claim: %w", err)
		}
	}
	if !indexPresent || !bytes.Equal(indexBytes, canonicalBytes) {
		if err := store.PutValueTx(ctx, tx, namespace, indexKey, canonicalBytes); err != nil {
			return nil, fmt.Errorf("write frozen expected run index: %w", err)
		}
	}
	return canonicalBytes, nil
}

func createFrozenAttemptLease(
	ctx context.Context,
	tx pgx.Tx,
	request FrozenAttemptAdmission,
) (RunAttemptLease, error) {
	query := `WITH db_clock AS MATERIALIZED (
			SELECT statement_timestamp() AS db_now
		)
		INSERT INTO weave_run_attempt_leases (
			workspace_id,run_id,attempt_generation,attempt_id,
			graph_name,run_started_at,attempt_started_at,state,
			heartbeat_at,lease_expires_at,claim_id,claim_expires_at,
			last_error_code,retry_count
		)
		SELECT $1,$2,$3,$4,$5,$6,db_now,'active',db_now,
			db_now+$7::interval,NULL,NULL,NULL,0
		FROM db_clock
		ON CONFLICT (workspace_id,run_id) DO NOTHING
		RETURNING ` + attemptLeaseColumns
	lease, err := scanAttemptLease(tx.QueryRow(
		ctx,
		query,
		request.Record.WorkspaceID,
		request.Record.RunID,
		request.AttemptGeneration,
		request.AttemptID,
		request.GraphName,
		request.RunStartedAt,
		request.LeaseTTL.String(),
	))
	if err == nil {
		return lease, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RunAttemptLease{}, fmt.Errorf("create frozen attempt lease: %w", err)
	}
	existing, present, err := NewPGTerminalStateStore().ReadAttemptLeaseForUpdate(
		ctx, tx, request.Record.WorkspaceID, request.Record.RunID,
	)
	if err != nil {
		return RunAttemptLease{}, fmt.Errorf("read frozen attempt lease conflict: %w", err)
	}
	if !present {
		return RunAttemptLease{}, fmt.Errorf("frozen attempt lease disappeared after conflict")
	}
	if existing.AttemptGeneration != request.AttemptGeneration {
		return RunAttemptLease{}, fmt.Errorf(
			"%w: durable generation %d differs from requested generation %d",
			ErrFrozenAttemptSuperseded,
			existing.AttemptGeneration,
			request.AttemptGeneration,
		)
	}
	if existing.AttemptID == request.AttemptID &&
		existing.GraphName == request.GraphName &&
		existing.RunStartedAt == request.RunStartedAt &&
		existing.State == AttemptLeaseActive &&
		existing.AttemptStartedAt.Equal(existing.HeartbeatAt) &&
		existing.LeaseExpiresAt.After(existing.HeartbeatAt) &&
		existing.ClaimID == nil && existing.ClaimExpiresAt == nil &&
		existing.LastErrorCode == nil && existing.RetryCount == 0 {
		return existing, nil
	}
	return RunAttemptLease{}, fmt.Errorf(
		"%w for workspace %q run %q",
		ErrFreshAttemptLeaseConflict,
		request.Record.WorkspaceID,
		request.Record.RunID,
	)
}
