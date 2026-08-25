package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) RecordFixedWorkflowAdmissionDenial(
	ctx context.Context,
	attempt FixedWorkflowAdmissionDenialAttempt,
) (*FixedWorkflowAdmissionDenialRecord, error) {
	if err := validateFixedWorkflowAdmissionDenialAttempt(attempt); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin fixed workflow admission denial audit: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	record := &FixedWorkflowAdmissionDenialRecord{
		WorkspaceID:         attempt.WorkspaceID,
		WorkflowID:          attempt.WorkflowID,
		WorkflowVersion:     attempt.WorkflowVersion,
		TriggerType:         attempt.TriggerType,
		AdmissionAttemptKey: attempt.AdmissionAttemptKey,
		ReasonCode:          attempt.ReasonCode,
		DecidedAt:           s.clock.Now().UTC().Truncate(time.Microsecond),
	}
	inserted := false
	err = tx.QueryRow(ctx, `
		INSERT INTO weave_workflow_admission_denials (
			workspace_id, workflow_id, workflow_version, trigger_type,
			admission_attempt_key, reason_code, decided_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (
			workspace_id, workflow_id, workflow_version, trigger_type,
			admission_attempt_key
		) DO NOTHING
		RETURNING true
	`,
		record.WorkspaceID,
		record.WorkflowID,
		record.WorkflowVersion,
		record.TriggerType,
		record.AdmissionAttemptKey,
		record.ReasonCode,
		record.DecidedAt,
	).Scan(&inserted)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			SELECT reason_code, decided_at
			FROM weave_workflow_admission_denials
			WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
			  AND trigger_type=$4 AND admission_attempt_key=$5
		`,
			record.WorkspaceID,
			record.WorkflowID,
			record.WorkflowVersion,
			record.TriggerType,
			record.AdmissionAttemptKey,
		).Scan(&record.ReasonCode, &record.DecidedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("persist fixed workflow admission denial audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit fixed workflow admission denial audit: %w", err)
	}
	return record, nil
}

func validateFixedWorkflowAdmissionDenialAttempt(attempt FixedWorkflowAdmissionDenialAttempt) error {
	if attempt.WorkspaceID == "" || attempt.WorkflowID == "" || attempt.WorkflowVersion < 1 ||
		attempt.TriggerType == "" || attempt.AdmissionAttemptKey == "" {
		return errors.New("fixed workflow admission denial identity is required")
	}
	for _, identity := range []string{
		attempt.WorkspaceID,
		attempt.WorkflowID,
		attempt.TriggerType,
		attempt.AdmissionAttemptKey,
	} {
		if identity != strings.TrimSpace(identity) {
			return errors.New("fixed workflow admission denial identity must be trimmed")
		}
	}
	if attempt.TriggerType != "schedule" && attempt.TriggerType != "manual" &&
		attempt.TriggerType != "api" &&
		attempt.TriggerType != "event" && attempt.TriggerType != "session" {
		return errors.New("fixed workflow admission denial trigger type is invalid")
	}
	if attempt.ReasonCode != FixedWorkflowAdmissionTeamWorkerDisabled &&
		attempt.ReasonCode != FixedWorkflowAdmissionVersionBlocked {
		return errors.New("fixed workflow admission denial reason is invalid")
	}
	return nil
}
