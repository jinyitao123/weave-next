package runtimes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Operations of a device control session (weave-workbench decision 009). The
// session itself is a task; each operation is a row whose params and result
// live only until delivered, after which the row is the audit record.

var (
	ErrDeviceOperationConflict = errors.New("device operation id reused with different content")
	ErrDeviceOperationNotFound = errors.New("device operation not found")
)

const (
	DeviceOperationQueued    = "queued"
	DeviceOperationDelivered = "delivered"
	DeviceOperationSucceeded = "succeeded"
	DeviceOperationFailed    = "failed"
	DeviceOperationUnknown   = "unknown"
	DeviceOperationStopped   = "stopped"
)

// DeviceOperation is one operation and, while undelivered, its params or result.
type DeviceOperation struct {
	WorkspaceID   string
	SessionID     string
	RuntimeID     string
	UserID        string
	OpID          string
	Kind          string
	RequestDigest string
	Audit         json.RawMessage
	Params        json.RawMessage
	Result        json.RawMessage
	Status        string
	Error         string
	CreatedAt     time.Time
	DeliveredAt   *time.Time
	CompletedAt   *time.Time
}

const deviceOperationColumns = `workspace_id, session_id, runtime_id, user_id, op_id, kind, request_digest,
	audit, params, result, status, error, created_at, delivered_at, completed_at`

func scanDeviceOperation(row rowScanner) (*DeviceOperation, error) {
	var op DeviceOperation
	err := row.Scan(&op.WorkspaceID, &op.SessionID, &op.RuntimeID, &op.UserID, &op.OpID, &op.Kind, &op.RequestDigest,
		&op.Audit, &op.Params, &op.Result, &op.Status, &op.Error, &op.CreatedAt, &op.DeliveredAt, &op.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDeviceOperationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read device operation: %w", err)
	}
	return &op, nil
}

// SetDeviceFacts records what a device reported about itself.
func (s *Store) SetDeviceFacts(ctx context.Context, workspaceID, id string, facts json.RawMessage) error {
	if len(facts) == 0 {
		facts = json.RawMessage(`{}`)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE weave_runtimes SET device_facts=$3, updated_at=$4
		WHERE workspace_id=$1 AND id=$2 AND device_owner_user_id IS NOT NULL AND deleted_at IS NULL`,
		workspaceID, id, facts, s.now())
	if err != nil {
		return fmt.Errorf("record device facts: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: device %q", ErrRuntimeUnavailable, id)
	}
	return nil
}

// QueueDeviceOperation records an operation once. The same op id with the
// same request digest returns the recorded operation unchanged; a different
// digest is a conflict, so a retry can never become a second, different act.
func (s *Store) QueueDeviceOperation(ctx context.Context, op DeviceOperation) (*DeviceOperation, bool, error) {
	if op.SessionID == "" || op.OpID == "" || op.Kind == "" || op.RequestDigest == "" {
		return nil, false, errors.New("device operation needs a session, an id, a kind and a digest")
	}
	if len(op.Audit) == 0 {
		op.Audit = json.RawMessage(`{}`)
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO weave_device_operations
			(id, workspace_id, session_id, runtime_id, user_id, op_id, kind, request_digest, audit, params, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'queued', $11)
		ON CONFLICT (session_id, op_id) DO NOTHING
	`, uuid.New(), op.WorkspaceID, op.SessionID, op.RuntimeID, op.UserID, op.OpID, op.Kind, op.RequestDigest, op.Audit, op.Params, s.now())
	if err != nil {
		return nil, false, fmt.Errorf("queue device operation: %w", err)
	}
	recorded, err := s.DeviceOperation(ctx, op.SessionID, op.OpID)
	if err != nil {
		return nil, false, err
	}
	if recorded.RequestDigest != op.RequestDigest || recorded.Kind != op.Kind {
		return nil, false, ErrDeviceOperationConflict
	}
	return recorded, tag.RowsAffected() == 1, nil
}

// DeviceOperation reads one operation of a session.
func (s *Store) DeviceOperation(ctx context.Context, sessionID, opID string) (*DeviceOperation, error) {
	return scanDeviceOperation(s.pool.QueryRow(ctx, `SELECT `+deviceOperationColumns+`
		FROM weave_device_operations WHERE session_id=$1 AND op_id=$2`, sessionID, opID))
}

// NextDeviceOperation hands the session's oldest queued operation to the
// device and clears its params, or returns nil when there is none.
func (s *Store) NextDeviceOperation(ctx context.Context, sessionID string) (*DeviceOperation, error) {
	op, err := scanDeviceOperation(s.pool.QueryRow(ctx, `
		WITH picked AS (
			SELECT id, params FROM weave_device_operations
			WHERE session_id=$1 AND status='queued'
			ORDER BY created_at, op_id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE weave_device_operations o
		SET status='delivered', delivered_at=$2, params=NULL
		FROM picked WHERE o.id=picked.id
		RETURNING o.workspace_id, o.session_id, o.runtime_id, o.user_id, o.op_id, o.kind, o.request_digest,
			o.audit, picked.params, o.result, o.status, o.error, o.created_at, o.delivered_at, o.completed_at
	`, sessionID, s.now()))
	if errors.Is(err, ErrDeviceOperationNotFound) {
		return nil, nil
	}
	return op, err
}

// CompleteDeviceOperation stores the device's outcome for a delivered
// operation; audit adds content-free facts about the outcome.
func (s *Store) CompleteDeviceOperation(ctx context.Context, sessionID, opID, status string, result json.RawMessage, message string, audit json.RawMessage) error {
	if status != DeviceOperationSucceeded && status != DeviceOperationFailed {
		return fmt.Errorf("device operation status %q", status)
	}
	if len(audit) == 0 {
		audit = json.RawMessage(`{}`)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE weave_device_operations
		SET status=$3, result=$4, error=$5, audit=audit || $6::jsonb, completed_at=$7
		WHERE session_id=$1 AND op_id=$2 AND status='delivered'`,
		sessionID, opID, status, result, message, audit, s.now())
	if err != nil {
		return fmt.Errorf("complete device operation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDeviceOperationNotFound
	}
	return nil
}

// TakeDeviceOperationResult returns a finished operation with its result
// and clears the stored result, so only the audit record remains.
func (s *Store) TakeDeviceOperationResult(ctx context.Context, sessionID, opID string) (*DeviceOperation, error) {
	return scanDeviceOperation(s.pool.QueryRow(ctx, `
		WITH taken AS (
			SELECT id, result FROM weave_device_operations
			WHERE session_id=$1 AND op_id=$2 FOR UPDATE
		)
		UPDATE weave_device_operations o SET result=NULL
		FROM taken WHERE o.id=taken.id
		RETURNING o.workspace_id, o.session_id, o.runtime_id, o.user_id, o.op_id, o.kind, o.request_digest,
			o.audit, o.params, taken.result, o.status, o.error, o.created_at, o.delivered_at, o.completed_at
	`, sessionID, opID))
}

// EndDeviceOperations settles a session that has ended: what was handed to
// the device without an answer is unknown, never assumed undone, and what was
// never handed over is stopped. Params are cleared either way.
func (s *Store) EndDeviceOperations(ctx context.Context, sessionID string) error {
	now := s.now()
	if _, err := s.pool.Exec(ctx, `UPDATE weave_device_operations
		SET status=CASE status WHEN 'delivered' THEN 'unknown' ELSE 'stopped' END, params=NULL, completed_at=$2
		WHERE session_id=$1 AND status IN ('queued','delivered')`, sessionID, now); err != nil {
		return fmt.Errorf("end device operations: %w", err)
	}
	return nil
}

// RecordDeviceSessionEnd keeps the first reason a session ended for.
func (s *Store) RecordDeviceSessionEnd(ctx context.Context, workspaceID, sessionID, reason string) error {
	if _, err := s.pool.Exec(ctx, `INSERT INTO weave_device_session_ends (session_id, workspace_id, reason, ended_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT (session_id) DO NOTHING`, sessionID, workspaceID, reason, s.now()); err != nil {
		return fmt.Errorf("record device session end: %w", err)
	}
	return nil
}

// DeviceSessionEndReason reads why a session ended, or "" when not recorded.
func (s *Store) DeviceSessionEndReason(ctx context.Context, sessionID string) (string, error) {
	var reason string
	err := s.pool.QueryRow(ctx, `SELECT reason FROM weave_device_session_ends WHERE session_id=$1`, sessionID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read device session end: %w", err)
	}
	return reason, nil
}

// DeviceLastActivity is when the session last queued an operation, for the
// idle limit; zero when it has none.
func (s *Store) DeviceLastActivity(ctx context.Context, sessionID string) (time.Time, error) {
	var last *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT max(created_at) FROM weave_device_operations WHERE session_id=$1`, sessionID).Scan(&last); err != nil {
		return time.Time{}, fmt.Errorf("read device activity: %w", err)
	}
	if last == nil {
		return time.Time{}, nil
	}
	return *last, nil
}
