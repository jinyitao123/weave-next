package loomruntime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrFreshAttemptLeaseConflict = errors.New("fresh attempt lease conflict")

type attemptLeaseDecodeError struct{ cause error }

func (err *attemptLeaseDecodeError) Error() string {
	return "decode attempt lease: " + err.cause.Error()
}

func (err *attemptLeaseDecodeError) Unwrap() error { return err.cause }

type AttemptLeaseState string

const (
	AttemptLeaseActive      AttemptLeaseState = "active"
	AttemptLeaseYielded     AttemptLeaseState = "yielded"
	AttemptLeaseClosed      AttemptLeaseState = "closed"
	AttemptLeaseReconciling AttemptLeaseState = "reconciling"
	AttemptLeaseReconciled  AttemptLeaseState = "reconciled"
)

type RunAttemptLease struct {
	WorkspaceID       string
	RunID             string
	AttemptGeneration int64
	AttemptID         uuid.UUID
	GraphName         string
	RunStartedAt      string
	AttemptStartedAt  time.Time
	State             AttemptLeaseState
	HeartbeatAt       time.Time
	LeaseExpiresAt    time.Time
	ClaimID           *uuid.UUID
	ClaimExpiresAt    *time.Time
	LastErrorCode     *string
	RetryCount        int64
}

func ValidateRunAttemptLease(l RunAttemptLease) error {
	for field, value := range map[string]string{"workspace_id": l.WorkspaceID, "run_id": l.RunID, "graph_name": l.GraphName, "run_started_at": l.RunStartedAt} {
		if value == "" {
			return fmt.Errorf("%s must be non-empty", field)
		}
	}
	if l.AttemptGeneration < 1 {
		return fmt.Errorf("attempt_generation must be >= 1")
	}
	if l.AttemptID == uuid.Nil {
		return fmt.Errorf("attempt_id must be non-zero")
	}
	if err := canonicalRunStartedAt(l.RunStartedAt); err != nil {
		return err
	}
	if err := enumValue("state", l.State, AttemptLeaseActive, AttemptLeaseYielded, AttemptLeaseClosed, AttemptLeaseReconciling, AttemptLeaseReconciled); err != nil {
		return err
	}
	if l.AttemptStartedAt.IsZero() {
		return fmt.Errorf("attempt_started_at must be non-zero")
	}
	if l.HeartbeatAt.IsZero() {
		return fmt.Errorf("heartbeat_at must be non-zero")
	}
	if l.LeaseExpiresAt.IsZero() {
		return fmt.Errorf("lease_expires_at must be non-zero")
	}
	if l.AttemptStartedAt.After(l.HeartbeatAt) || l.HeartbeatAt.After(l.LeaseExpiresAt) {
		return fmt.Errorf("time_order requires attempt_started_at <= heartbeat_at <= lease_expires_at")
	}
	if l.State == AttemptLeaseReconciling {
		if l.ClaimID == nil || l.ClaimExpiresAt == nil || *l.ClaimID == uuid.Nil {
			return fmt.Errorf("claim_shape reconciling requires non-zero id and expiry")
		}
	} else if l.ClaimID != nil || l.ClaimExpiresAt != nil {
		return fmt.Errorf("claim_shape forbids claim outside reconciling")
	}
	if l.LastErrorCode != nil && *l.LastErrorCode == "" {
		return fmt.Errorf("last_error_code must be non-empty when present")
	}
	if l.RetryCount < 0 {
		return fmt.Errorf("retry_count must be non-negative")
	}
	return nil
}

const attemptLeaseColumns = "workspace_id,run_id,attempt_generation,attempt_id,graph_name,run_started_at,attempt_started_at,state,heartbeat_at,lease_expires_at,claim_id,claim_expires_at,last_error_code,retry_count"

func scanAttemptLease(row rowScanner) (RunAttemptLease, error) {
	var l RunAttemptLease
	var state string
	err := row.Scan(&l.WorkspaceID, &l.RunID, &l.AttemptGeneration, &l.AttemptID, &l.GraphName, &l.RunStartedAt, &l.AttemptStartedAt, &state, &l.HeartbeatAt, &l.LeaseExpiresAt, &l.ClaimID, &l.ClaimExpiresAt, &l.LastErrorCode, &l.RetryCount)
	if err != nil {
		return RunAttemptLease{}, err
	}
	l.State = AttemptLeaseState(state)
	if err := ValidateRunAttemptLease(l); err != nil {
		return RunAttemptLease{}, &attemptLeaseDecodeError{cause: err}
	}
	return l, nil
}

func (*PGTerminalStateStore) ReadAttemptLeaseForUpdate(ctx context.Context, tx pgx.Tx, workspaceID, runID string) (RunAttemptLease, bool, error) {
	if tx == nil {
		return RunAttemptLease{}, false, fmt.Errorf("tx must be non-nil")
	}
	if workspaceID == "" || runID == "" {
		return RunAttemptLease{}, false, fmt.Errorf("physical key must be non-empty")
	}
	l, err := scanAttemptLease(tx.QueryRow(ctx, "SELECT "+attemptLeaseColumns+" FROM weave_run_attempt_leases WHERE workspace_id=$1 AND run_id=$2 FOR UPDATE", workspaceID, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunAttemptLease{}, false, nil
	}
	if err != nil {
		return RunAttemptLease{}, false, fmt.Errorf("read weave_run_attempt_leases: %w", err)
	}
	return l, true, nil
}

// CreateInitialAttemptLease inserts the generation-one active owner for a
// fresh run. It never overwrites an existing attempt lease.
func (store *PGTerminalStateStore) CreateInitialAttemptLease(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	runID string,
	attemptID uuid.UUID,
	graphName string,
	runStartedAt string,
	ttl time.Duration,
) (RunAttemptLease, error) {
	if tx == nil {
		return RunAttemptLease{}, fmt.Errorf("tx must be non-nil")
	}
	if workspaceID == "" || runID == "" {
		return RunAttemptLease{}, fmt.Errorf("physical key must be non-empty")
	}
	if attemptID == uuid.Nil {
		return RunAttemptLease{}, fmt.Errorf("attempt_id must be non-zero")
	}
	if graphName == "" {
		return RunAttemptLease{}, fmt.Errorf("graph_name must be non-empty")
	}
	if err := canonicalRunStartedAt(runStartedAt); err != nil {
		return RunAttemptLease{}, err
	}
	if ttl <= 0 {
		return RunAttemptLease{}, fmt.Errorf("lease ttl must be positive")
	}

	query := `WITH db_clock AS MATERIALIZED (
			SELECT statement_timestamp() AS db_now
		)
		INSERT INTO weave_run_attempt_leases (
			workspace_id, run_id, attempt_generation, attempt_id,
			graph_name, run_started_at,
			attempt_started_at, state, heartbeat_at, lease_expires_at,
			claim_id, claim_expires_at, last_error_code, retry_count
		)
		SELECT
			$1, $2, 1, $3, $4, $5,
			db_now, 'active', db_now, db_now + $6::interval,
			NULL, NULL, NULL, 0
		FROM db_clock
		ON CONFLICT (workspace_id, run_id) DO NOTHING
		RETURNING ` + attemptLeaseColumns
	lease, err := scanAttemptLease(tx.QueryRow(
		ctx,
		query,
		workspaceID,
		runID,
		attemptID,
		graphName,
		runStartedAt,
		ttl.String(),
	))
	if err == nil {
		return lease, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RunAttemptLease{}, fmt.Errorf("create initial attempt lease: %w", err)
	}

	existing, present, err := store.ReadAttemptLeaseForUpdate(ctx, tx, workspaceID, runID)
	if err != nil {
		return RunAttemptLease{}, fmt.Errorf("read conflicting initial attempt lease: %w", err)
	}
	if !present {
		return RunAttemptLease{}, fmt.Errorf("initial attempt lease disappeared after conflict")
	}
	if existing.AttemptGeneration == 1 &&
		existing.AttemptID == attemptID &&
		existing.GraphName == graphName &&
		existing.RunStartedAt == runStartedAt &&
		existing.State == AttemptLeaseActive &&
		existing.AttemptStartedAt.Equal(existing.HeartbeatAt) &&
		existing.LeaseExpiresAt.After(existing.HeartbeatAt) &&
		existing.ClaimID == nil &&
		existing.ClaimExpiresAt == nil &&
		existing.LastErrorCode == nil &&
		existing.RetryCount == 0 {
		return existing, nil
	}
	return RunAttemptLease{}, fmt.Errorf(
		"%w for workspace %q run %q",
		ErrFreshAttemptLeaseConflict,
		workspaceID,
		runID,
	)
}

// ActivateResumeAttemptLease atomically replaces one exact yielded owner with
// the next active generation. A false result means the durable owner no longer
// matches; callers must not treat that as an idempotent replay.
func (*PGTerminalStateStore) ActivateResumeAttemptLease(
	ctx context.Context,
	tx pgx.Tx,
	owner AttemptLeaseOwner,
	nextAttemptID uuid.UUID,
	ttl time.Duration,
) (RunAttemptLease, bool, error) {
	if tx == nil {
		return RunAttemptLease{}, false, fmt.Errorf("tx must be non-nil")
	}
	if err := validateAttemptLeaseOwner(owner); err != nil {
		return RunAttemptLease{}, false, err
	}
	if owner.Generation == math.MaxInt64 {
		return RunAttemptLease{}, false, fmt.Errorf("attempt generation overflow")
	}
	if nextAttemptID == uuid.Nil {
		return RunAttemptLease{}, false, fmt.Errorf("next attempt_id must be non-zero")
	}
	if nextAttemptID == owner.AttemptID {
		return RunAttemptLease{}, false, fmt.Errorf("next attempt_id must differ from current owner")
	}
	if ttl <= 0 {
		return RunAttemptLease{}, false, fmt.Errorf("lease ttl must be positive")
	}

	query := `WITH db_clock AS MATERIALIZED (
			SELECT statement_timestamp() AS db_now
		)
		UPDATE weave_run_attempt_leases AS lease
		SET
			attempt_generation = lease.attempt_generation + 1,
			attempt_id = $7,
			attempt_started_at = db_clock.db_now,
			state = 'active',
			heartbeat_at = db_clock.db_now,
			lease_expires_at = db_clock.db_now + $8::interval,
			claim_id = NULL,
			claim_expires_at = NULL,
			last_error_code = NULL,
			retry_count = 0
		FROM db_clock
		WHERE lease.workspace_id = $1
			AND lease.run_id = $2
			AND lease.attempt_generation = $3
			AND lease.attempt_id = $4
			AND lease.graph_name = $5
			AND lease.run_started_at = $6
			AND lease.state = 'yielded'
		RETURNING ` + attemptLeaseColumns
	lease, err := scanAttemptLease(tx.QueryRow(
		ctx,
		query,
		owner.WorkspaceID,
		owner.RunID,
		owner.Generation,
		owner.AttemptID,
		owner.GraphName,
		owner.RunStartedAt,
		nextAttemptID,
		ttl.String(),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunAttemptLease{}, false, nil
	}
	if err != nil {
		return RunAttemptLease{}, false, fmt.Errorf("activate resume attempt lease: %w", err)
	}
	return lease, true, nil
}

// MarkAttemptLeaseYielded hands off one exact active owner without changing
// any other lease field.
func (*PGTerminalStateStore) MarkAttemptLeaseYielded(
	ctx context.Context,
	tx pgx.Tx,
	owner AttemptLeaseOwner,
) (RunAttemptLease, bool, error) {
	if tx == nil {
		return RunAttemptLease{}, false, fmt.Errorf("tx must be non-nil")
	}
	if err := validateAttemptLeaseOwner(owner); err != nil {
		return RunAttemptLease{}, false, err
	}
	query := `UPDATE weave_run_attempt_leases
		SET state = 'yielded'
		WHERE workspace_id = $1
			AND run_id = $2
			AND attempt_generation = $3
			AND attempt_id = $4
			AND graph_name = $5
			AND run_started_at = $6
			AND state = 'active'
		RETURNING ` + attemptLeaseColumns
	lease, err := scanAttemptLease(tx.QueryRow(
		ctx,
		query,
		owner.WorkspaceID,
		owner.RunID,
		owner.Generation,
		owner.AttemptID,
		owner.GraphName,
		owner.RunStartedAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunAttemptLease{}, false, nil
	}
	if err != nil {
		return RunAttemptLease{}, false, fmt.Errorf("mark attempt lease yielded: %w", err)
	}
	return lease, true, nil
}

// TransitionNormalAttemptLease applies only the exact state changes owned by
// the normal terminal transaction. Replays are handled by the coordinator
// without issuing an UPDATE.
func (*PGTerminalStateStore) TransitionNormalAttemptLease(
	ctx context.Context,
	tx pgx.Tx,
	owner AttemptLeaseOwner,
	from AttemptLeaseState,
	to AttemptLeaseState,
) (RunAttemptLease, bool, error) {
	if tx == nil {
		return RunAttemptLease{}, false, fmt.Errorf("tx must be non-nil")
	}
	if err := validateAttemptLeaseOwner(owner); err != nil {
		return RunAttemptLease{}, false, err
	}
	validTransition := (from == AttemptLeaseActive &&
		(to == AttemptLeaseYielded || to == AttemptLeaseClosed)) ||
		(from == AttemptLeaseReconciling && to == AttemptLeaseClosed) ||
		(from == AttemptLeaseReconciled && to == AttemptLeaseClosed)
	if !validTransition {
		return RunAttemptLease{}, false, fmt.Errorf(
			"normal terminal lease transition %q -> %q is forbidden",
			from,
			to,
		)
	}

	query := `UPDATE weave_run_attempt_leases
		SET state = $7,
			claim_id = NULL,
			claim_expires_at = NULL,
			last_error_code = NULL
		WHERE workspace_id = $1
			AND run_id = $2
			AND attempt_generation = $3
			AND attempt_id = $4
			AND graph_name = $5
			AND run_started_at = $6
			AND state = $8
		RETURNING ` + attemptLeaseColumns
	lease, err := scanAttemptLease(tx.QueryRow(
		ctx,
		query,
		owner.WorkspaceID,
		owner.RunID,
		owner.Generation,
		owner.AttemptID,
		owner.GraphName,
		owner.RunStartedAt,
		string(to),
		string(from),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunAttemptLease{}, false, nil
	}
	if err != nil {
		return RunAttemptLease{}, false, fmt.Errorf(
			"transition normal terminal attempt lease: %w",
			err,
		)
	}
	return lease, true, nil
}

func (*PGTerminalStateStore) RenewAttemptLease(ctx context.Context, tx pgx.Tx, workspaceID, runID string, generation int64, attemptID uuid.UUID, ttl time.Duration) (time.Time, time.Time, bool, error) {
	if tx == nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("tx must be non-nil")
	}
	if workspaceID == "" || runID == "" || generation < 1 || attemptID == uuid.Nil || ttl <= 0 {
		return time.Time{}, time.Time{}, false, fmt.Errorf("renew arguments invalid")
	}
	var now, expiry time.Time
	err := tx.QueryRow(ctx, `WITH db_clock AS MATERIALIZED (SELECT statement_timestamp() AS db_now) UPDATE weave_run_attempt_leases AS lease SET heartbeat_at=db_clock.db_now, lease_expires_at=db_clock.db_now+$5::interval FROM db_clock WHERE lease.workspace_id=$1 AND lease.run_id=$2 AND lease.attempt_generation=$3 AND lease.attempt_id=$4 AND lease.state='active' AND lease.lease_expires_at>db_clock.db_now RETURNING db_clock.db_now,lease.lease_expires_at`, workspaceID, runID, generation, attemptID, ttl.String()).Scan(&now, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, false, fmt.Errorf("renew attempt lease: %w", err)
	}
	return now, expiry, true, nil
}
