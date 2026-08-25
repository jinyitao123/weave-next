package sessionexec

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jinyitao123/loom/contract"
	"github.com/jinyitao123/weave/internal/kernel/workflow/machine"
)

type PGStore struct{}

func NewPGStore() *PGStore { return &PGStore{} }

type rowScanner interface{ Scan(...any) error }

type storedSessionExecutionLease struct {
	SessionExecutionLease
	acquireEventID string
}

const sessionExecutionLeaseColumns = `workspace_id,project_id,user_id,lead_avatar_id,session_id,
	lease_epoch,state,active_run_id,run_snapshot_id,controller_kind,controller_id,
	yield_kind,resume_token_hash,yield_generation,input_schema,acquire_event_id,
	expires_at,created_at,updated_at,closed_at,close_reason`

const sessionOutboxColumns = `workspace_id,project_id,user_id,lead_avatar_id,session_id,event_id,
	lease_epoch,active_run_id,run_snapshot_id,role,content,metadata,delivery_state,
	delivery_attempts,claim_owner,claim_expires_at,created_at,delivered_at,last_error`

func scanStoredSessionExecutionLease(row rowScanner) (storedSessionExecutionLease, error) {
	var stored storedSessionExecutionLease
	lease := &stored.SessionExecutionLease
	var state, controllerKind, yieldKind string
	var yieldGeneration *int64
	var closeReason *string
	var projectID *string
	if err := row.Scan(
		&lease.Key.WorkspaceID,
		&projectID,
		&lease.Key.UserID,
		&lease.Key.LeadAvatarID,
		&lease.Key.SessionID,
		&lease.LeaseEpoch,
		&state,
		&lease.ActiveRunID,
		&lease.RunSnapshotID,
		&controllerKind,
		&lease.ControllerID,
		&yieldKind,
		&lease.ResumeTokenHash,
		&yieldGeneration,
		&lease.InputSchema,
		&stored.acquireEventID,
		&lease.ExpiresAt,
		&lease.CreatedAt,
		&lease.UpdatedAt,
		&lease.ClosedAt,
		&closeReason,
	); err != nil {
		return storedSessionExecutionLease{}, err
	}
	lease.State = LeaseState(state)
	if projectID != nil {
		lease.ProjectID = *projectID
	}
	lease.ControllerKind = ControllerKind(controllerKind)
	lease.YieldKind = YieldKind(yieldKind)
	if yieldGeneration != nil {
		lease.YieldGeneration = *yieldGeneration
	}
	if closeReason != nil {
		reason := CloseReason(*closeReason)
		lease.CloseReason = &reason
	}
	if stored.acquireEventID == "" {
		return storedSessionExecutionLease{}, fmt.Errorf("decode session execution lease: acquire_event_id must be non-empty")
	}
	if err := ValidateSessionExecutionLease(*lease); err != nil {
		return storedSessionExecutionLease{}, fmt.Errorf("decode session execution lease: %w", err)
	}
	return stored, nil
}

func scanSessionExecutionLease(row rowScanner) (SessionExecutionLease, error) {
	stored, err := scanStoredSessionExecutionLease(row)
	if err != nil {
		return SessionExecutionLease{}, err
	}
	return stored.SessionExecutionLease, nil
}

func scanOutboxRecord(row rowScanner) (OutboxRecord, error) {
	var record OutboxRecord
	var deliveryState string
	var projectID *string
	if err := row.Scan(
		&record.Key.WorkspaceID,
		&projectID,
		&record.Key.UserID,
		&record.Key.LeadAvatarID,
		&record.Key.SessionID,
		&record.EventID,
		&record.LeaseEpoch,
		&record.ActiveRunID,
		&record.RunSnapshotID,
		&record.Role,
		&record.Content,
		&record.Metadata,
		&deliveryState,
		&record.DeliveryAttempts,
		&record.ClaimOwner,
		&record.ClaimExpiresAt,
		&record.CreatedAt,
		&record.DeliveredAt,
		&record.LastError,
	); err != nil {
		return OutboxRecord{}, err
	}
	record.DeliveryState = DeliveryState(deliveryState)
	if projectID != nil {
		record.ProjectID = *projectID
	}
	return record, nil
}

func requireTx(tx pgx.Tx) error {
	if tx == nil {
		return fmt.Errorf("tx must be non-nil")
	}
	return nil
}

func keyArgs(key SessionKey) []any {
	return []any{key.WorkspaceID, key.UserID, key.LeadAvatarID, key.SessionID}
}

func readLease(ctx context.Context, tx pgx.Tx, key SessionKey, forUpdate bool) (SessionExecutionLease, error) {
	query := `SELECT ` + sessionExecutionLeaseColumns + `
		FROM weave_session_execution_leases
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	lease, err := scanSessionExecutionLease(tx.QueryRow(ctx, query, keyArgs(key)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionExecutionLease{}, ErrLeaseNotFound
	}
	if err != nil {
		return SessionExecutionLease{}, fmt.Errorf("read session execution lease: %w", err)
	}
	return lease, nil
}

func lockSessionKey(ctx context.Context, tx pgx.Tx, key SessionKey) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(
		current_schema() || chr(31) || $1 || chr(31) || $2 || chr(31) || $3 || chr(31) || $4, 0
	))`, keyArgs(key)...)
	if err != nil {
		return fmt.Errorf("lock session execution lease key: %w", err)
	}
	return nil
}

func validateAcquireRequest(req AcquireRequest) error {
	if err := ValidateSessionKey(req.Key); err != nil {
		return err
	}
	if req.EventID == "" || req.ActiveRunID == "" || req.RunSnapshotID == "" || req.ControllerID == "" {
		return fmt.Errorf("acquire identity must be non-empty")
	}
	if err := validateEnum("controller_kind", req.ControllerKind, ControllerLead, ControllerWorker); err != nil {
		return err
	}
	if req.TTL <= 0 {
		return fmt.Errorf("lease TTL must be positive")
	}
	return nil
}

func sameAcquireIdentity(lease storedSessionExecutionLease, req AcquireRequest) bool {
	return lease.ProjectID == req.ProjectID &&
		lease.ActiveRunID == req.ActiveRunID &&
		lease.RunSnapshotID == req.RunSnapshotID &&
		lease.ControllerKind == req.ControllerKind &&
		lease.ControllerID == req.ControllerID
}

func (*PGStore) AcquireTx(ctx context.Context, tx pgx.Tx, req AcquireRequest) (AcquireResult, error) {
	if err := requireTx(tx); err != nil {
		return AcquireResult{}, err
	}
	if err := validateAcquireRequest(req); err != nil {
		return AcquireResult{}, err
	}
	if err := lockSessionKey(ctx, tx, req.Key); err != nil {
		return AcquireResult{}, err
	}

	stored, err := scanStoredSessionExecutionLease(tx.QueryRow(ctx, `SELECT `+sessionExecutionLeaseColumns+`
		FROM weave_session_execution_leases
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4
		FOR UPDATE`, keyArgs(req.Key)...))
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrLeaseNotFound
	} else if err != nil {
		err = fmt.Errorf("read session execution lease: %w", err)
	}
	if errors.Is(err, ErrLeaseNotFound) {
		query := `WITH db_clock AS MATERIALIZED (SELECT clock_timestamp() AS db_now)
			INSERT INTO weave_session_execution_leases (
				workspace_id,project_id,user_id,lead_avatar_id,session_id,lease_epoch,state,
				active_run_id,run_snapshot_id,controller_kind,controller_id,yield_kind,
				resume_token_hash,yield_generation,input_schema,acquire_event_id,
				expires_at,created_at,updated_at,closed_at,close_reason
			)
				SELECT $1,NULLIF($2,''),$3,$4,$5,1,'active',$6,$7,$8,$9,'none',NULL,NULL,NULL,$10,
					db_now+$11::interval,db_now,db_now,NULL,NULL
			FROM db_clock
			RETURNING ` + sessionExecutionLeaseColumns
		created, insertErr := scanSessionExecutionLease(tx.QueryRow(ctx, query,
			req.Key.WorkspaceID, req.ProjectID, req.Key.UserID, req.Key.LeadAvatarID, req.Key.SessionID,
			req.ActiveRunID, req.RunSnapshotID, string(req.ControllerKind), req.ControllerID,
			req.EventID, req.TTL.String(),
		))
		if insertErr != nil {
			return AcquireResult{}, fmt.Errorf("insert session execution lease: %w", insertErr)
		}
		return AcquireResult{Disposition: AcquireGranted, Lease: created}, nil
	}
	if err != nil {
		return AcquireResult{}, err
	}
	if stored.ProjectID != req.ProjectID {
		return AcquireResult{}, fmt.Errorf("%w: acquire project identity differs", ErrLeaseStateConflict)
	}

	if stored.acquireEventID == req.EventID {
		if !sameAcquireIdentity(stored, req) {
			return AcquireResult{}, fmt.Errorf("%w: acquire event identity differs", ErrLeaseStateConflict)
		}
		return AcquireResult{Disposition: AcquireReplay, Lease: stored.SessionExecutionLease}, nil
	}
	lease := stored.SessionExecutionLease
	switch lease.State {
	case LeaseStateActive:
		return AcquireResult{Disposition: AcquireBusy, Lease: lease}, nil
	case LeaseStateParked:
		return AcquireResult{Disposition: AcquireRouteToParked, Lease: lease}, nil
	case LeaseStateClosed:
		query := `WITH db_clock AS MATERIALIZED (SELECT clock_timestamp() AS db_now)
			UPDATE weave_session_execution_leases AS lease SET
				lease_epoch=lease.lease_epoch+1,state='active',active_run_id=$5,
				run_snapshot_id=$6,controller_kind=$7,controller_id=$8,yield_kind='none',
				resume_token_hash=NULL,yield_generation=NULL,input_schema=NULL,
				acquire_event_id=$9,expires_at=db_clock.db_now+$10::interval,
				created_at=db_clock.db_now,updated_at=db_clock.db_now,
				closed_at=NULL,close_reason=NULL
			FROM db_clock
			WHERE lease.workspace_id=$1 AND lease.user_id=$2 AND lease.lead_avatar_id=$3
				AND lease.session_id=$4 AND lease.state='closed'
			RETURNING ` + sessionExecutionLeaseColumns
		created, updateErr := scanSessionExecutionLease(tx.QueryRow(ctx, query,
			req.Key.WorkspaceID, req.Key.UserID, req.Key.LeadAvatarID, req.Key.SessionID,
			req.ActiveRunID, req.RunSnapshotID, string(req.ControllerKind), req.ControllerID,
			req.EventID, req.TTL.String(),
		))
		if updateErr != nil {
			return AcquireResult{}, fmt.Errorf("reactivate closed session execution lease: %w", updateErr)
		}
		return AcquireResult{Disposition: AcquireGranted, Lease: created}, nil
	default:
		return AcquireResult{}, fmt.Errorf("%w: unsupported state %q", ErrLeaseStateConflict, lease.State)
	}
}

func (*PGStore) Get(ctx context.Context, tx pgx.Tx, key SessionKey) (SessionExecutionLease, error) {
	if err := requireTx(tx); err != nil {
		return SessionExecutionLease{}, err
	}
	if err := ValidateSessionKey(key); err != nil {
		return SessionExecutionLease{}, err
	}
	return readLease(ctx, tx, key, false)
}

func classifyLeaseCAS(ctx context.Context, tx pgx.Tx, key SessionKey, epoch int64, runID string, allowed ...LeaseState) error {
	lease, err := readLease(ctx, tx, key, true)
	if err != nil {
		return err
	}
	if lease.LeaseEpoch != epoch {
		return ErrLeaseEpochFenced
	}
	if lease.ActiveRunID != runID {
		return ErrLeaseRunMismatch
	}
	for _, state := range allowed {
		if lease.State == state {
			return ErrLeaseStateConflict
		}
	}
	return ErrLeaseStateConflict
}

// HashResumeToken returns the v1 domain-separated digest persisted by Park.
func HashResumeToken(key SessionKey, runID string, generation int64, token string) []byte {
	digest := sha256.New()
	for _, part := range []string{
		"weave:session-resume-token:v1",
		key.WorkspaceID,
		key.UserID,
		key.LeadAvatarID,
		key.SessionID,
		runID,
		fmt.Sprintf("%d", generation),
		token,
	} {
		_, _ = digest.Write([]byte(part))
		_, _ = digest.Write([]byte{0})
	}
	return digest.Sum(nil)
}

func validateParkRequest(req ParkRequest) error {
	if err := ValidateSessionKey(req.Key); err != nil {
		return err
	}
	if req.LeaseEpoch < 1 || req.ActiveRunID == "" || req.YieldGeneration < 1 ||
		len(req.ResumeTokenHash) != sha256.Size || req.ExpiresAt.IsZero() {
		return fmt.Errorf("park identity is invalid")
	}
	if err := validateEnum("yield_kind", req.YieldKind, YieldInput); err != nil {
		return err
	}
	if problems := machine.ValidateRuntimeInputSchema(req.InputSchema); len(problems) > 0 {
		return &ResumeInputValidationError{Problems: problems}
	}
	return nil
}

// Park transitions the current active writer to a typed durable wait point.
func (*PGStore) Park(
	ctx context.Context,
	tx pgx.Tx,
	req ParkRequest,
) (SessionExecutionLease, error) {
	if err := requireTx(tx); err != nil {
		return SessionExecutionLease{}, err
	}
	if err := validateParkRequest(req); err != nil {
		return SessionExecutionLease{}, err
	}
	query := `WITH db_clock AS MATERIALIZED (SELECT statement_timestamp() AS db_now)
		UPDATE weave_session_execution_leases AS lease SET
			state='parked',yield_kind=$7,resume_token_hash=$8,yield_generation=$9,
			input_schema=$10::jsonb,expires_at=$11,updated_at=db_clock.db_now
		FROM db_clock
		WHERE lease.workspace_id=$1 AND lease.user_id=$2 AND lease.lead_avatar_id=$3
			AND lease.session_id=$4 AND lease.lease_epoch=$5 AND lease.active_run_id=$6
			AND lease.state='active' AND $11>db_clock.db_now
		RETURNING ` + sessionExecutionLeaseColumns
	lease, err := scanSessionExecutionLease(tx.QueryRow(
		ctx,
		query,
		req.Key.WorkspaceID,
		req.Key.UserID,
		req.Key.LeadAvatarID,
		req.Key.SessionID,
		req.LeaseEpoch,
		req.ActiveRunID,
		string(req.YieldKind),
		req.ResumeTokenHash,
		req.YieldGeneration,
		req.InputSchema,
		req.ExpiresAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionExecutionLease{}, classifyLeaseCAS(
			ctx, tx, req.Key, req.LeaseEpoch, req.ActiveRunID, LeaseStateActive,
		)
	}
	if err != nil {
		return SessionExecutionLease{}, fmt.Errorf("park session execution lease: %w", err)
	}
	return lease, nil
}

func validateResumeRequest(req ResumeRequest) error {
	if err := ValidateSessionKey(req.Key); err != nil {
		return err
	}
	if req.LeaseEpoch < 1 || req.ActiveRunID == "" || req.ResumeToken == "" ||
		req.YieldGeneration < 1 {
		return fmt.Errorf("resume identity is invalid")
	}
	if err := validateEnum("yield_kind", req.YieldKind, YieldInput); err != nil {
		return err
	}
	if len(req.Input) == 0 {
		return &ResumeInputValidationError{Problems: []machine.RuntimeSchemaProblem{{
			Code: machine.CodeJSONInvalid, Message: "resume input must be valid JSON",
		}}}
	}
	return nil
}

// ValidateAndActivate validates the frozen wait envelope and consumes its
// generation in the same row lock used by expiry.
func (*PGStore) ValidateAndActivate(
	ctx context.Context,
	tx pgx.Tx,
	req ResumeRequest,
) (ResumeResult, error) {
	if err := requireTx(tx); err != nil {
		return ResumeResult{}, err
	}
	if err := validateResumeRequest(req); err != nil {
		return ResumeResult{}, err
	}
	lease, err := readLease(ctx, tx, req.Key, true)
	if err != nil {
		return ResumeResult{}, err
	}
	switch {
	case lease.LeaseEpoch != req.LeaseEpoch:
		return ResumeResult{}, ErrLeaseEpochFenced
	case lease.ActiveRunID != req.ActiveRunID:
		return ResumeResult{}, ErrLeaseRunMismatch
	case lease.State != LeaseStateParked:
		return ResumeResult{}, ErrYieldGenerationConflict
	case lease.YieldKind != req.YieldKind:
		return ResumeResult{}, ErrYieldKindMismatch
	case lease.YieldGeneration != req.YieldGeneration:
		return ResumeResult{}, ErrYieldGenerationConflict
	}
	expected := HashResumeToken(
		req.Key, req.ActiveRunID, req.YieldGeneration, req.ResumeToken,
	)
	if len(lease.ResumeTokenHash) != len(expected) ||
		subtle.ConstantTimeCompare(lease.ResumeTokenHash, expected) != 1 {
		return ResumeResult{}, ErrResumeTokenInvalid
	}
	validatedInput, problems := machine.ValidateRuntimeInput(lease.InputSchema, req.Input)
	if len(problems) > 0 {
		return ResumeResult{}, &ResumeInputValidationError{Problems: problems}
	}
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&dbNow); err != nil {
		return ResumeResult{}, fmt.Errorf("read resume database clock: %w", err)
	}
	if !lease.ExpiresAt.After(dbNow) {
		return ResumeResult{}, ErrLeaseExpired
	}
	query := `UPDATE weave_session_execution_leases SET
		state='active',yield_kind='none',resume_token_hash=NULL,yield_generation=NULL,
		input_schema=NULL,updated_at=clock_timestamp()
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4
			AND lease_epoch=$5 AND active_run_id=$6 AND state='parked'
			AND yield_kind=$7 AND resume_token_hash=$8 AND yield_generation=$9
			AND expires_at>$10
		RETURNING ` + sessionExecutionLeaseColumns
	activated, err := scanSessionExecutionLease(tx.QueryRow(
		ctx,
		query,
		req.Key.WorkspaceID,
		req.Key.UserID,
		req.Key.LeadAvatarID,
		req.Key.SessionID,
		req.LeaseEpoch,
		req.ActiveRunID,
		string(req.YieldKind),
		lease.ResumeTokenHash,
		req.YieldGeneration,
		dbNow,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return ResumeResult{}, ErrYieldGenerationConflict
	}
	if err != nil {
		return ResumeResult{}, fmt.Errorf("activate parked session execution lease: %w", err)
	}
	return ResumeResult{Lease: activated, ValidatedInput: validatedInput}, nil
}

func validateTransferRequest(req TransferControllerRequest) error {
	if err := ValidateSessionKey(req.Key); err != nil {
		return err
	}
	if req.LeaseEpoch < 1 || req.ActiveRunID == "" || req.ExpectedControllerID == "" || req.NewControllerID == "" {
		return fmt.Errorf("controller transfer identity is invalid")
	}
	if err := validateEnum("expected_controller_kind", req.ExpectedControllerKind, ControllerLead, ControllerWorker); err != nil {
		return err
	}
	if err := validateEnum("new_controller_kind", req.NewControllerKind, ControllerLead, ControllerWorker); err != nil {
		return err
	}
	if req.ExpiresAt.IsZero() {
		return fmt.Errorf("expires_at must be non-zero")
	}
	return nil
}

func (*PGStore) TransferControllerTx(ctx context.Context, tx pgx.Tx, req TransferControllerRequest) (SessionExecutionLease, error) {
	if err := requireTx(tx); err != nil {
		return SessionExecutionLease{}, err
	}
	if err := validateTransferRequest(req); err != nil {
		return SessionExecutionLease{}, err
	}
	query := `WITH db_clock AS MATERIALIZED (SELECT statement_timestamp() AS db_now)
		UPDATE weave_session_execution_leases AS lease SET
			controller_kind=$7,controller_id=$8,lease_epoch=lease.lease_epoch+1,
			expires_at=$9,updated_at=db_clock.db_now
		FROM db_clock
		WHERE lease.workspace_id=$1 AND lease.user_id=$2 AND lease.lead_avatar_id=$3
			AND lease.session_id=$4 AND lease.lease_epoch=$5 AND lease.active_run_id=$6
			AND lease.state IN ('active','parked') AND lease.controller_kind=$10
			AND lease.controller_id=$11 AND $9>db_clock.db_now
		RETURNING ` + sessionExecutionLeaseColumns
	lease, err := scanSessionExecutionLease(tx.QueryRow(ctx, query,
		req.Key.WorkspaceID, req.Key.UserID, req.Key.LeadAvatarID, req.Key.SessionID,
		req.LeaseEpoch, req.ActiveRunID, string(req.NewControllerKind), req.NewControllerID,
		req.ExpiresAt, string(req.ExpectedControllerKind), req.ExpectedControllerID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionExecutionLease{}, classifyLeaseCAS(ctx, tx, req.Key, req.LeaseEpoch, req.ActiveRunID, LeaseStateActive, LeaseStateParked)
	}
	if err != nil {
		return SessionExecutionLease{}, fmt.Errorf("transfer session execution lease controller: %w", err)
	}
	return lease, nil
}

func (*PGStore) RenewTx(ctx context.Context, tx pgx.Tx, req RenewRequest) (SessionExecutionLease, error) {
	if err := requireTx(tx); err != nil {
		return SessionExecutionLease{}, err
	}
	if err := ValidateSessionKey(req.Key); err != nil {
		return SessionExecutionLease{}, err
	}
	if req.LeaseEpoch < 1 || req.ActiveRunID == "" || req.ExpiresAt.IsZero() {
		return SessionExecutionLease{}, fmt.Errorf("renew identity is invalid")
	}
	query := `WITH db_clock AS MATERIALIZED (SELECT statement_timestamp() AS db_now)
		UPDATE weave_session_execution_leases AS lease SET expires_at=$7,updated_at=db_clock.db_now
		FROM db_clock
		WHERE lease.workspace_id=$1 AND lease.user_id=$2 AND lease.lead_avatar_id=$3
			AND lease.session_id=$4 AND lease.lease_epoch=$5 AND lease.active_run_id=$6
			AND lease.state IN ('active','parked') AND $7>db_clock.db_now
			AND $7>=lease.expires_at
		RETURNING ` + sessionExecutionLeaseColumns
	lease, err := scanSessionExecutionLease(tx.QueryRow(ctx, query,
		req.Key.WorkspaceID, req.Key.UserID, req.Key.LeadAvatarID, req.Key.SessionID,
		req.LeaseEpoch, req.ActiveRunID, req.ExpiresAt,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionExecutionLease{}, classifyLeaseCAS(ctx, tx, req.Key, req.LeaseEpoch, req.ActiveRunID, LeaseStateActive, LeaseStateParked)
	}
	if err != nil {
		return SessionExecutionLease{}, fmt.Errorf("renew session execution lease: %w", err)
	}
	return lease, nil
}

func validateFailRequest(req FailRequest) error {
	if err := ValidateSessionKey(req.Key); err != nil {
		return err
	}
	if req.LeaseEpoch < 1 || req.ActiveRunID == "" || req.RunSnapshotID == "" {
		return fmt.Errorf("failure close identity is invalid")
	}
	return nil
}

// FailTx closes the exact active writer after a failed terminal execution
// without creating an outbox event. Replaying the same failed close identity
// is idempotent; every other stale or conflicting identity remains fenced.
func (*PGStore) FailTx(ctx context.Context, tx pgx.Tx, req FailRequest) (SessionExecutionLease, error) {
	if err := requireTx(tx); err != nil {
		return SessionExecutionLease{}, err
	}
	if err := validateFailRequest(req); err != nil {
		return SessionExecutionLease{}, err
	}
	lease, err := readLease(ctx, tx, req.Key, true)
	if err != nil {
		return SessionExecutionLease{}, err
	}
	if lease.LeaseEpoch != req.LeaseEpoch {
		return SessionExecutionLease{}, ErrLeaseEpochFenced
	}
	if lease.ActiveRunID != req.ActiveRunID {
		return SessionExecutionLease{}, ErrLeaseRunMismatch
	}
	if lease.RunSnapshotID != req.RunSnapshotID {
		return SessionExecutionLease{}, ErrLeaseStateConflict
	}
	if lease.State == LeaseStateClosed && lease.CloseReason != nil && *lease.CloseReason == CloseFailed {
		return lease, nil
	}
	if lease.State != LeaseStateActive {
		return SessionExecutionLease{}, ErrLeaseStateConflict
	}
	query := `WITH db_clock AS MATERIALIZED (SELECT clock_timestamp() AS db_now)
		UPDATE weave_session_execution_leases AS lease SET
			state='closed',yield_kind='none',resume_token_hash=NULL,yield_generation=NULL,
			input_schema=NULL,updated_at=db_clock.db_now,closed_at=db_clock.db_now,
			close_reason='failed'
		FROM db_clock
		WHERE lease.workspace_id=$1 AND lease.user_id=$2 AND lease.lead_avatar_id=$3
			AND lease.session_id=$4 AND lease.lease_epoch=$5 AND lease.active_run_id=$6
			AND lease.run_snapshot_id=$7 AND lease.state='active'
		RETURNING ` + sessionExecutionLeaseColumns
	closed, err := scanSessionExecutionLease(tx.QueryRow(ctx, query,
		req.Key.WorkspaceID, req.Key.UserID, req.Key.LeadAvatarID, req.Key.SessionID,
		req.LeaseEpoch, req.ActiveRunID, req.RunSnapshotID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionExecutionLease{}, classifyLeaseCAS(
			ctx, tx, req.Key, req.LeaseEpoch, req.ActiveRunID, LeaseStateActive,
		)
	}
	if err != nil {
		return SessionExecutionLease{}, fmt.Errorf("fail session execution lease: %w", err)
	}
	return closed, nil
}

func validateOutboxMessage(message FinalOutboxMessage) error {
	if err := ValidateSessionKey(message.Key); err != nil {
		return err
	}
	if message.EventID == "" || message.LeaseEpoch < 1 || message.ActiveRunID == "" || message.RunSnapshotID == "" {
		return fmt.Errorf("session outbox identity is invalid")
	}
	if message.Role != "assistant" && message.Role != "event" {
		return fmt.Errorf("session outbox role %q is invalid", message.Role)
	}
	return validateJSON("metadata", message.Metadata)
}

func validateFinalMessage(message FinalOutboxMessage) error {
	if err := validateOutboxMessage(message); err != nil {
		return err
	}
	if message.Role != "assistant" {
		return fmt.Errorf("final outbox role %q is invalid", message.Role)
	}
	if strings.TrimSpace(message.Content) == "" {
		return fmt.Errorf("final outbox content must be non-blank")
	}
	return nil
}

func jsonEquivalent(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func outboxEquivalent(record OutboxRecord, message FinalOutboxMessage) bool {
	return record.EventID == message.EventID &&
		record.Key == message.Key &&
		record.LeaseEpoch == message.LeaseEpoch &&
		record.ActiveRunID == message.ActiveRunID &&
		record.RunSnapshotID == message.RunSnapshotID &&
		record.Role == message.Role &&
		record.Content == message.Content &&
		jsonEquivalent(record.Metadata, message.Metadata)
}

func (*PGStore) CommitFinalTx(ctx context.Context, tx pgx.Tx, message FinalOutboxMessage) (SessionExecutionLease, error) {
	if err := requireTx(tx); err != nil {
		return SessionExecutionLease{}, err
	}
	if err := validateFinalMessage(message); err != nil {
		return SessionExecutionLease{}, err
	}
	lease, err := readLease(ctx, tx, message.Key, true)
	if err != nil {
		return SessionExecutionLease{}, err
	}

	outboxQuery := `SELECT ` + sessionOutboxColumns + ` FROM weave_session_outbox
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4 AND event_id=$5`
	record, outboxErr := scanOutboxRecord(tx.QueryRow(ctx, outboxQuery,
		message.Key.WorkspaceID, message.Key.UserID, message.Key.LeadAvatarID, message.Key.SessionID, message.EventID,
	))
	if outboxErr == nil {
		if !outboxEquivalent(record, message) {
			return SessionExecutionLease{}, ErrOutboxConflict
		}
		return lease, nil
	}
	if !errors.Is(outboxErr, pgx.ErrNoRows) {
		return SessionExecutionLease{}, fmt.Errorf("read session outbox idempotency fact: %w", outboxErr)
	}
	if lease.LeaseEpoch != message.LeaseEpoch {
		return SessionExecutionLease{}, ErrLeaseEpochFenced
	}
	if lease.ActiveRunID != message.ActiveRunID {
		return SessionExecutionLease{}, ErrLeaseRunMismatch
	}
	if lease.State != LeaseStateActive || lease.RunSnapshotID != message.RunSnapshotID {
		return SessionExecutionLease{}, ErrLeaseStateConflict
	}

	insertQuery := `WITH db_clock AS MATERIALIZED (SELECT clock_timestamp() AS db_now)
			INSERT INTO weave_session_outbox (
				workspace_id,project_id,user_id,lead_avatar_id,session_id,event_id,lease_epoch,
				active_run_id,run_snapshot_id,role,content,metadata,delivery_state,
				delivery_attempts,claim_owner,claim_expires_at,created_at,delivered_at,last_error
			) SELECT $1,NULLIF($2,''),$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,'pending',0,NULL,NULL,db_now,NULL,NULL
			FROM db_clock`
	if _, err := tx.Exec(ctx, insertQuery,
		message.Key.WorkspaceID, lease.ProjectID, message.Key.UserID, message.Key.LeadAvatarID, message.Key.SessionID,
		message.EventID, message.LeaseEpoch, message.ActiveRunID, message.RunSnapshotID,
		message.Role, message.Content, message.Metadata,
	); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return SessionExecutionLease{}, fmt.Errorf("%w: single final event uniqueness", ErrOutboxConflict)
		}
		return SessionExecutionLease{}, fmt.Errorf("insert session outbox: %w", err)
	}
	closeQuery := `WITH db_clock AS MATERIALIZED (SELECT clock_timestamp() AS db_now)
		UPDATE weave_session_execution_leases AS lease SET
			state='closed',yield_kind='none',resume_token_hash=NULL,yield_generation=NULL,
			input_schema=NULL,updated_at=db_clock.db_now,closed_at=db_clock.db_now,
			close_reason='final_committed'
		FROM db_clock
		WHERE lease.workspace_id=$1 AND lease.user_id=$2 AND lease.lead_avatar_id=$3
			AND lease.session_id=$4 AND lease.lease_epoch=$5 AND lease.active_run_id=$6
			AND lease.run_snapshot_id=$7 AND lease.state='active'
		RETURNING ` + sessionExecutionLeaseColumns
	closed, err := scanSessionExecutionLease(tx.QueryRow(ctx, closeQuery,
		message.Key.WorkspaceID, message.Key.UserID, message.Key.LeadAvatarID, message.Key.SessionID,
		message.LeaseEpoch, message.ActiveRunID, message.RunSnapshotID,
	))
	if err != nil {
		return SessionExecutionLease{}, fmt.Errorf("close session execution lease after outbox insert: %w", err)
	}
	return closed, nil
}

type sessionProjectionMetadata struct {
	SessionKey      string             `json:"session_key"`
	SessionMessages []contract.Message `json:"session_messages,omitempty"`
}

func isolatedPayloadDigest(kind string, payload ...[]byte) []byte {
	digest := sha256.New()
	_, _ = digest.Write([]byte("weave:isolated-side-effect:v1"))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(kind))
	for _, part := range payload {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write(part)
	}
	return digest.Sum(nil)
}

func appendIsolationAudit(
	ctx context.Context,
	tx pgx.Tx,
	lease SessionExecutionLease,
	observedEpoch int64,
	activeRunID string,
	kind string,
	digest []byte,
) error {
	detail, _ := json.Marshal(map[string]any{
		"schema_version": 1,
		"side_effect":    kind,
	})
	return NewPGStore().IsolatedAppendAuditTx(ctx, tx, IsolatedSessionAuditEvent{
		ID:              fmt.Sprintf("isolated:%s:%d:%x", kind, observedEpoch, digest),
		Key:             lease.Key,
		ObservedEpoch:   observedEpoch,
		CurrentEpoch:    lease.LeaseEpoch,
		ActiveRunID:     activeRunID,
		EventKind:       "late_write_isolated",
		IsolationReason: kind + "_fenced",
		PayloadDigest:   digest,
		Detail:          detail,
	})
}

// AppendSessionMessageTx projects one delivered outbox event into the Loom
// session namespace exactly once.
func (*PGStore) AppendSessionMessageTx(
	ctx context.Context,
	tx pgx.Tx,
	message FinalOutboxMessage,
) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateOutboxMessage(message); err != nil {
		return err
	}
	var metadata sessionProjectionMetadata
	if err := json.Unmarshal(message.Metadata, &metadata); err != nil {
		return fmt.Errorf("decode session projection metadata: %w", err)
	}
	if metadata.SessionKey == "" {
		return fmt.Errorf("session projection metadata requires session_key")
	}
	record, err := scanOutboxRecord(tx.QueryRow(ctx, `SELECT `+sessionOutboxColumns+`
		FROM weave_session_outbox
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4
			AND event_id=$5`,
		message.Key.WorkspaceID, message.Key.UserID, message.Key.LeadAvatarID,
		message.Key.SessionID, message.EventID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOutboxConflict
	}
	if err != nil {
		return fmt.Errorf("verify session projection outbox: %w", err)
	}
	if !outboxEquivalent(record, message) {
		return ErrOutboxConflict
	}
	eventMarkerKey := strings.Join([]string{
		message.Key.WorkspaceID,
		message.Key.UserID,
		message.Key.LeadAvatarID,
		message.Key.SessionID,
		message.EventID,
	}, "\x1f")
	var markerPresent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM loom_store WHERE namespace='session_event' AND key=$1
	)`, eventMarkerKey).Scan(&markerPresent); err != nil {
		return fmt.Errorf("read session projection marker: %w", err)
	}
	if markerPresent {
		return nil
	}
	lease, err := readLease(ctx, tx, message.Key, true)
	if err != nil {
		return err
	}
	if lease.LeaseEpoch != message.LeaseEpoch || lease.ActiveRunID != message.ActiveRunID {
		digest := isolatedPayloadDigest(
			"session_projection", []byte(message.Content), message.Metadata,
		)
		if auditErr := appendIsolationAudit(
			ctx, tx, lease, message.LeaseEpoch, message.ActiveRunID,
			"session_projection", digest,
		); auditErr != nil {
			return auditErr
		}
		return ErrLeaseEpochFenced
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO loom_store(namespace,key,value,updated_at)
		VALUES ('session_event',$1,$2,clock_timestamp())
		ON CONFLICT(namespace,key) DO NOTHING`, eventMarkerKey, []byte("1"))
	if err != nil {
		return fmt.Errorf("insert session projection marker: %w", err)
	}
	if inserted.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(
		hashtextextended('session',0) # hashtextextended($1,0)
	)`, metadata.SessionKey); err != nil {
		return fmt.Errorf("lock session projection: %w", err)
	}
	var current []byte
	err = tx.QueryRow(ctx, `SELECT value FROM loom_store
		WHERE namespace='session' AND key=$1`, metadata.SessionKey).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read session projection: %w", err)
	}
	var messages []contract.Message
	if len(current) > 0 {
		if err := json.Unmarshal(current, &messages); err != nil {
			return fmt.Errorf("decode session projection: %w", err)
		}
	}
	add := metadata.SessionMessages
	if len(add) == 0 {
		add = []contract.Message{{Role: message.Role, Content: message.Content}}
	}
	messages = append(messages, add...)
	encoded, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("encode session projection: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loom_store(namespace,key,value,updated_at)
		VALUES ('session',$1,$2,clock_timestamp())
		ON CONFLICT(namespace,key) DO UPDATE
		SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at`,
		metadata.SessionKey, encoded,
	); err != nil {
		return fmt.Errorf("write session projection: %w", err)
	}
	return nil
}

type memoryPutPayload struct {
	Namespace string         `json:"namespace"`
	Content   string         `json:"content"`
	Embedding []float32      `json:"embedding"`
	Metadata  map[string]any `json:"metadata"`
}

// PutMemoryTx performs the final epoch check immediately before vector insert.
func (*PGStore) PutMemoryTx(
	ctx context.Context,
	tx pgx.Tx,
	key SessionKey,
	epoch int64,
	payload json.RawMessage,
) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := ValidateSessionKey(key); err != nil {
		return err
	}
	var value memoryPutPayload
	if err := json.Unmarshal(payload, &value); err != nil {
		return fmt.Errorf("decode fenced memory payload: %w", err)
	}
	activeRunID, _ := value.Metadata["active_run_id"].(string)
	if epoch < 1 || activeRunID == "" || value.Namespace == "" ||
		value.Content == "" || len(value.Embedding) == 0 {
		return fmt.Errorf("fenced memory payload identity is invalid")
	}
	lease, err := readLease(ctx, tx, key, true)
	if err != nil {
		return err
	}
	if lease.LeaseEpoch != epoch || lease.ActiveRunID != activeRunID {
		digest := isolatedPayloadDigest("memory", []byte(value.Namespace), []byte(value.Content))
		if auditErr := appendIsolationAudit(
			ctx, tx, lease, epoch, activeRunID, "memory", digest,
		); auditErr != nil {
			return auditErr
		}
		return ErrLeaseEpochFenced
	}
	if value.Metadata == nil {
		value.Metadata = map[string]any{}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loom_memory(
		namespace,content,embedding,metadata
	) VALUES ($1,$2,$3,$4)`,
		value.Namespace, value.Content, vectorLiteral(value.Embedding), value.Metadata,
	); err != nil {
		return fmt.Errorf("insert fenced memory: %w", err)
	}
	return nil
}

func vectorLiteral(values []float32) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			builder.WriteByte(',')
		}
		_, _ = fmt.Fprintf(&builder, "%g", value)
	}
	builder.WriteByte(']')
	return builder.String()
}

func (*PGStore) ListExpired(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]SessionExecutionLease, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	if now.IsZero() || limit <= 0 {
		return nil, fmt.Errorf("expiry scan arguments are invalid")
	}
	rows, err := tx.Query(ctx, `SELECT `+sessionExecutionLeaseColumns+`
		FROM weave_session_execution_leases
		WHERE state IN ('active','parked') AND expires_at<=$1
		ORDER BY expires_at,workspace_id,user_id,lead_avatar_id,session_id
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, fmt.Errorf("list expired session execution leases: %w", err)
	}
	defer rows.Close()
	var leases []SessionExecutionLease
	for rows.Next() {
		lease, scanErr := scanSessionExecutionLease(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan expired session execution lease: %w", scanErr)
		}
		leases = append(leases, lease)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired session execution leases: %w", err)
	}
	return leases, nil
}

func (*PGStore) ExpireOneTx(ctx context.Context, tx pgx.Tx, key SessionKey, observedEpoch int64, now time.Time) (ExpireResult, error) {
	if err := requireTx(tx); err != nil {
		return ExpireResult{}, err
	}
	if err := ValidateSessionKey(key); err != nil {
		return ExpireResult{}, err
	}
	if observedEpoch < 1 || now.IsZero() {
		return ExpireResult{}, fmt.Errorf("expire identity is invalid")
	}
	lease, err := readLease(ctx, tx, key, true)
	if err != nil {
		return ExpireResult{}, err
	}
	result := ExpireResult{Key: key, OldEpoch: observedEpoch, FencedEpoch: lease.LeaseEpoch, ActiveRunID: lease.ActiveRunID}
	if lease.LeaseEpoch != observedEpoch || (lease.State != LeaseStateActive && lease.State != LeaseStateParked) || lease.ExpiresAt.After(now) {
		return result, nil
	}

	query := `UPDATE weave_session_execution_leases SET
		lease_epoch=lease_epoch+1,state='closed',yield_kind='none',resume_token_hash=NULL,
		yield_generation=NULL,input_schema=NULL,updated_at=$6,closed_at=$6,close_reason='expired'
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4
			AND lease_epoch=$5 AND state IN ('active','parked') AND expires_at<=$6
		RETURNING ` + sessionExecutionLeaseColumns
	closed, err := scanSessionExecutionLease(tx.QueryRow(ctx, query,
		key.WorkspaceID, key.UserID, key.LeadAvatarID, key.SessionID, observedEpoch, now,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return ExpireResult{}, fmt.Errorf("expire session execution lease: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_session_execution_audit (
				id,workspace_id,project_id,user_id,lead_avatar_id,session_id,observed_epoch,current_epoch,
				active_run_id,event_kind,isolation_reason,payload_digest,detail,created_at
			) VALUES (
				$1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,$9,'lease_expired',NULL,NULL,
				jsonb_build_object('run_state','cancel_requested'),$10
			)`, fmt.Sprintf("lease_expired:%d", observedEpoch),
		key.WorkspaceID, closed.ProjectID, key.UserID, key.LeadAvatarID, key.SessionID,
		observedEpoch, closed.LeaseEpoch, closed.ActiveRunID, now)
	if err != nil {
		return ExpireResult{}, fmt.Errorf("append session execution expiry audit: %w", err)
	}
	result.FencedEpoch = closed.LeaseEpoch
	result.Changed = true
	return result, nil
}

func validateAuditEvent(event SessionAuditEvent) error {
	if err := ValidateSessionKey(event.Key); err != nil {
		return err
	}
	if event.ID == "" || event.ObservedEpoch < 1 || event.ActiveRunID == "" || event.EventKind == "" {
		return fmt.Errorf("session audit identity is invalid")
	}
	if event.EventKind == "late_write_isolated" {
		return fmt.Errorf("isolated late-write event requires IsolatedAppendAuditTx")
	}
	return validateJSON("audit detail", event.Detail)
}

func (*PGStore) FencedAppendAuditTx(ctx context.Context, tx pgx.Tx, event SessionAuditEvent) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateAuditEvent(event); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `INSERT INTO weave_session_execution_audit (
			id,workspace_id,project_id,user_id,lead_avatar_id,session_id,observed_epoch,current_epoch,
			active_run_id,event_kind,isolation_reason,payload_digest,detail,created_at
		) SELECT $1,$2,lease.project_id,$3,$4,$5,$6,lease.lease_epoch,$7,$8,$9,$10,$11::jsonb,clock_timestamp()
		FROM weave_session_execution_leases AS lease
		WHERE lease.workspace_id=$2 AND lease.user_id=$3 AND lease.lead_avatar_id=$4
		AND lease.session_id=$5 AND lease.lease_epoch=$6 AND lease.active_run_id=$7
		AND lease.state IN ('active','parked')`,
		event.ID, event.Key.WorkspaceID, event.Key.UserID, event.Key.LeadAvatarID, event.Key.SessionID,
		event.ObservedEpoch, event.ActiveRunID, event.EventKind, event.IsolationReason,
		event.PayloadDigest, event.Detail,
	)
	if err != nil {
		return fmt.Errorf("append fenced session execution audit: %w", err)
	}
	if command.RowsAffected() == 0 {
		return classifyLeaseCAS(ctx, tx, event.Key, event.ObservedEpoch, event.ActiveRunID, LeaseStateActive, LeaseStateParked)
	}
	return nil
}

func validateIsolatedAuditEvent(event IsolatedSessionAuditEvent) error {
	if err := ValidateSessionKey(event.Key); err != nil {
		return err
	}
	if event.ID == "" || event.ObservedEpoch < 1 || event.CurrentEpoch < 1 || event.ActiveRunID == "" {
		return fmt.Errorf("isolated session audit identity is invalid")
	}
	if event.EventKind != "late_write_isolated" {
		return fmt.Errorf("isolated audit event_kind must be late_write_isolated")
	}
	if event.IsolationReason == "" || len(event.PayloadDigest) == 0 {
		return fmt.Errorf("isolated audit reason and payload digest must be present")
	}
	return validateJSON("isolated audit detail", event.Detail)
}

func (*PGStore) IsolatedAppendAuditTx(ctx context.Context, tx pgx.Tx, event IsolatedSessionAuditEvent) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateIsolatedAuditEvent(event); err != nil {
		return err
	}
	lease, err := readLease(ctx, tx, event.Key, true)
	if err != nil {
		return err
	}
	if lease.LeaseEpoch != event.CurrentEpoch {
		return ErrLeaseEpochFenced
	}
	_, err = tx.Exec(ctx, `INSERT INTO weave_session_execution_audit (
			id,workspace_id,project_id,user_id,lead_avatar_id,session_id,observed_epoch,current_epoch,
			active_run_id,event_kind,isolation_reason,payload_digest,detail,created_at
		) VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,$9,'late_write_isolated',$10,$11,$12::jsonb,clock_timestamp())
		ON CONFLICT (workspace_id,user_id,lead_avatar_id,session_id,id) DO NOTHING`,
		event.ID, event.Key.WorkspaceID, lease.ProjectID, event.Key.UserID, event.Key.LeadAvatarID, event.Key.SessionID,
		event.ObservedEpoch, lease.LeaseEpoch, event.ActiveRunID, event.IsolationReason,
		event.PayloadDigest, event.Detail,
	)
	if err != nil {
		return fmt.Errorf("append isolated session execution audit: %w", err)
	}
	return nil
}

func (*PGStore) ClaimOutboxBatch(ctx context.Context, tx pgx.Tx, claimOwner string, claimExpiresAt time.Time, limit int) ([]OutboxRecord, error) {
	if err := requireTx(tx); err != nil {
		return nil, err
	}
	if claimOwner == "" || claimExpiresAt.IsZero() || limit <= 0 {
		return nil, fmt.Errorf("outbox claim arguments are invalid")
	}
	query := `WITH db_clock AS MATERIALIZED (SELECT statement_timestamp() AS db_now),
		candidate AS (
			SELECT outbox.workspace_id,outbox.user_id,outbox.lead_avatar_id,outbox.session_id,outbox.event_id
			FROM weave_session_outbox AS outbox,db_clock
			WHERE outbox.delivery_state='pending'
				OR (outbox.delivery_state='delivering' AND outbox.claim_expires_at<=db_clock.db_now)
			ORDER BY outbox.created_at,outbox.workspace_id,outbox.user_id,
				outbox.lead_avatar_id,outbox.session_id,outbox.event_id
			FOR UPDATE OF outbox SKIP LOCKED LIMIT $2
		)
		UPDATE weave_session_outbox AS outbox SET
			delivery_state='delivering',delivery_attempts=outbox.delivery_attempts+1,
			claim_owner=$1,claim_expires_at=$3,last_error=NULL
		FROM candidate,db_clock
		WHERE outbox.workspace_id=candidate.workspace_id AND outbox.user_id=candidate.user_id
			AND outbox.lead_avatar_id=candidate.lead_avatar_id
			AND outbox.session_id=candidate.session_id AND outbox.event_id=candidate.event_id
			AND $3>db_clock.db_now
		RETURNING ` + "outbox." + strings.ReplaceAll(sessionOutboxColumns, ",", ",outbox.")
	rows, err := tx.Query(ctx, query, claimOwner, limit, claimExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("claim session outbox batch: %w", err)
	}
	defer rows.Close()
	var records []OutboxRecord
	for rows.Next() {
		record, scanErr := scanOutboxRecord(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan claimed session outbox: %w", scanErr)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed session outbox: %w", err)
	}
	return records, nil
}

func validateOutboxMark(key SessionKey, eventID, claimOwner string, at time.Time) error {
	if err := ValidateSessionKey(key); err != nil {
		return err
	}
	if eventID == "" || claimOwner == "" || at.IsZero() {
		return fmt.Errorf("outbox delivery identity is invalid")
	}
	return nil
}

func (*PGStore) MarkOutboxDelivered(ctx context.Context, tx pgx.Tx, key SessionKey, eventID, claimOwner string, deliveredAt time.Time) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateOutboxMark(key, eventID, claimOwner, deliveredAt); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE weave_session_outbox SET
		delivery_state='delivered',claim_owner=NULL,claim_expires_at=NULL,
		delivered_at=$7,last_error=NULL
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4
			AND event_id=$5 AND delivery_state='delivering' AND claim_owner=$6`,
		key.WorkspaceID, key.UserID, key.LeadAvatarID, key.SessionID,
		eventID, claimOwner, deliveredAt,
	)
	if err != nil {
		return fmt.Errorf("mark session outbox delivered: %w", err)
	}
	if command.RowsAffected() == 0 {
		return ErrLeaseStateConflict
	}
	return nil
}

func (*PGStore) MarkOutboxFailed(ctx context.Context, tx pgx.Tx, key SessionKey, eventID, claimOwner string, retryAt time.Time, lastError string) error {
	if err := requireTx(tx); err != nil {
		return err
	}
	if err := validateOutboxMark(key, eventID, claimOwner, retryAt); err != nil {
		return err
	}
	if lastError == "" {
		return fmt.Errorf("last_error must be non-empty")
	}
	command, err := tx.Exec(ctx, `UPDATE weave_session_outbox SET
		claim_owner=NULL,claim_expires_at=$7,last_error=$8
		WHERE workspace_id=$1 AND user_id=$2 AND lead_avatar_id=$3 AND session_id=$4
			AND event_id=$5 AND delivery_state='delivering' AND claim_owner=$6`,
		key.WorkspaceID, key.UserID, key.LeadAvatarID, key.SessionID,
		eventID, claimOwner, retryAt, lastError,
	)
	if err != nil {
		return fmt.Errorf("mark session outbox failed: %w", err)
	}
	if command.RowsAffected() == 0 {
		return ErrLeaseStateConflict
	}
	return nil
}
