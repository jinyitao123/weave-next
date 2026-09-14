package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const admissionAuditColumns = `
	workspace_id, workflow_id, workflow_version, audit_id, idempotency_key,
	old_blocked, new_blocked, operator_id, reason, created_at
`

// SetBlocked atomically records and applies one idempotent admission command.
func (s *ArtifactStore) SetBlocked(
	ctx context.Context,
	change AdmissionChange,
) (*AdmissionResult, error) {
	requestHash, err := admissionCommandHash(change)
	if err != nil {
		return nil, fmt.Errorf("hash admission command: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin admission change: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var blocked bool
	err = tx.QueryRow(ctx, `
		SELECT blocked
		FROM weave_workflow_version_admission_statuses
		WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
		FOR UPDATE
	`,
		change.WorkspaceID,
		change.WorkflowID,
		change.WorkflowVersion,
	).Scan(&blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"%w: workflow %q version %d admission status",
			ErrNotFound,
			change.WorkflowID,
			change.WorkflowVersion,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("lock workflow version admission status: %w", err)
	}

	var (
		storedHash     string
		storedResponse []byte
	)
	err = tx.QueryRow(ctx, `
		SELECT request_hash, response
		FROM weave_workflow_version_admission_receipts
		WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
		  AND idempotency_key=$4
	`,
		change.WorkspaceID,
		change.WorkflowID,
		change.WorkflowVersion,
		change.IdempotencyKey,
	).Scan(&storedHash, &storedResponse)
	switch {
	case err == nil:
		if storedHash != requestHash {
			return nil, fmt.Errorf(
				"%w: key %q",
				ErrAdmissionIdempotencyConflict,
				change.IdempotencyKey,
			)
		}
		result, decodeErr := decodeAdmissionResult(storedResponse)
		if decodeErr != nil {
			return nil, fmt.Errorf("decode frozen admission response: %w", decodeErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit admission replay: %w", err)
		}
		return result, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("read admission receipt: %w", err)
	}

	result := &AdmissionResult{
		SchemaVersion: 1,
		OldBlocked:    blocked,
		NewBlocked:    change.DesiredBlocked,
		Changed:       blocked != change.DesiredBlocked,
	}
	if result.Changed {
		auditID := uuid.NewString()
		result.AuditID = &auditID
	}
	response, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode admission response: %w", err)
	}
	createdAt := s.clock.Now().Truncate(time.Microsecond)

	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_workflow_version_admission_receipts (
			workspace_id, workflow_id, workflow_version, idempotency_key,
			request_hash, response, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`,
		change.WorkspaceID,
		change.WorkflowID,
		change.WorkflowVersion,
		change.IdempotencyKey,
		requestHash,
		string(response),
		createdAt,
	); err != nil {
		return nil, fmt.Errorf("insert admission receipt: %w", err)
	}

	if result.Changed {
		if _, err := tx.Exec(ctx, `
			UPDATE weave_workflow_version_admission_statuses
			SET blocked=$4
			WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
		`,
			change.WorkspaceID,
			change.WorkflowID,
			change.WorkflowVersion,
			change.DesiredBlocked,
		); err != nil {
			return nil, fmt.Errorf("update workflow version admission status: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_workflow_version_admission_audits (
				workspace_id, workflow_id, workflow_version, audit_id,
				idempotency_key, old_blocked, new_blocked, operator_id,
				reason, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			change.WorkspaceID,
			change.WorkflowID,
			change.WorkflowVersion,
			*result.AuditID,
			change.IdempotencyKey,
			result.OldBlocked,
			result.NewBlocked,
			change.OperatorID,
			change.Reason,
			createdAt,
		); err != nil {
			return nil, fmt.Errorf("insert admission audit: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit admission change: %w", err)
	}
	return result, nil
}

// ListAdmissionAudit returns one version's transitions in stable chronological
// order.
func (s *ArtifactStore) ListAdmissionAudit(
	ctx context.Context,
	workspaceID, workflowID string,
	version int,
) ([]AdmissionAudit, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+admissionAuditColumns+`
		FROM weave_workflow_version_admission_audits
		WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
		ORDER BY created_at, audit_id COLLATE "C"
	`, workspaceID, workflowID, version)
	if err != nil {
		return nil, fmt.Errorf("list workflow version admission audit: %w", err)
	}
	defer rows.Close()

	audits := make([]AdmissionAudit, 0)
	for rows.Next() {
		var audit AdmissionAudit
		if err := rows.Scan(
			&audit.WorkspaceID,
			&audit.WorkflowID,
			&audit.WorkflowVersion,
			&audit.AuditID,
			&audit.IdempotencyKey,
			&audit.OldBlocked,
			&audit.NewBlocked,
			&audit.OperatorID,
			&audit.Reason,
			&audit.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan workflow version admission audit: %w", err)
		}
		audits = append(audits, audit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow version admission audit: %w", err)
	}
	return audits, nil
}

func admissionCommandHash(change AdmissionChange) (string, error) {
	operator, err := quoteJCSString(change.OperatorID)
	if err != nil {
		return "", fmt.Errorf("operator_id: %w", err)
	}
	reason, err := quoteJCSString(change.Reason)
	if err != nil {
		return "", fmt.Errorf("reason: %w", err)
	}

	var canonical strings.Builder
	canonical.WriteString(`{"desired_blocked":`)
	canonical.WriteString(strconv.FormatBool(change.DesiredBlocked))
	canonical.WriteString(`,"operator_id":`)
	canonical.WriteString(operator)
	canonical.WriteString(`,"reason":`)
	canonical.WriteString(reason)
	canonical.WriteString(`,"schema_version":1}`)
	sum := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(sum[:]), nil
}

func quoteJCSString(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("invalid UTF-8")
	}

	var quoted strings.Builder
	quoted.Grow(len(value) + 2)
	quoted.WriteByte('"')
	for _, current := range value {
		switch current {
		case '"', '\\':
			quoted.WriteByte('\\')
			quoted.WriteRune(current)
		case '\b':
			quoted.WriteString(`\b`)
		case '\t':
			quoted.WriteString(`\t`)
		case '\n':
			quoted.WriteString(`\n`)
		case '\f':
			quoted.WriteString(`\f`)
		case '\r':
			quoted.WriteString(`\r`)
		default:
			if current < 0x20 {
				fmt.Fprintf(&quoted, `\u%04x`, current)
			} else {
				quoted.WriteRune(current)
			}
		}
	}
	quoted.WriteByte('"')
	return quoted.String(), nil
}

func decodeAdmissionResult(raw []byte) (*AdmissionResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	required := []string{
		"schema_version",
		"old_blocked",
		"new_blocked",
		"changed",
		"audit_id",
	}
	if len(fields) != len(required) {
		return nil, fmt.Errorf("response has %d fields, want %d", len(fields), len(required))
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return nil, fmt.Errorf("response field %q is required", name)
		}
	}

	var result AdmissionResult
	if err := json.Unmarshal(fields["schema_version"], &result.SchemaVersion); err != nil {
		return nil, fmt.Errorf("decode schema_version: %w", err)
	}
	if err := json.Unmarshal(fields["old_blocked"], &result.OldBlocked); err != nil {
		return nil, fmt.Errorf("decode old_blocked: %w", err)
	}
	if err := json.Unmarshal(fields["new_blocked"], &result.NewBlocked); err != nil {
		return nil, fmt.Errorf("decode new_blocked: %w", err)
	}
	if err := json.Unmarshal(fields["changed"], &result.Changed); err != nil {
		return nil, fmt.Errorf("decode changed: %w", err)
	}
	if string(fields["audit_id"]) != "null" {
		var auditID string
		if err := json.Unmarshal(fields["audit_id"], &auditID); err != nil {
			return nil, fmt.Errorf("decode audit_id: %w", err)
		}
		result.AuditID = &auditID
	}

	if result.SchemaVersion != 1 {
		return nil, fmt.Errorf("schema_version = %d, want 1", result.SchemaVersion)
	}
	if result.Changed != (result.OldBlocked != result.NewBlocked) {
		return nil, errors.New("changed does not match blocked transition")
	}
	if result.Changed {
		if result.AuditID == nil || *result.AuditID == "" {
			return nil, errors.New("changed response requires audit_id")
		}
	} else if result.AuditID != nil {
		return nil, errors.New("unchanged response requires null audit_id")
	}
	return &result, nil
}
