package teambuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

var (
	ErrBlueprintPatchPlanningAttemptNotFound = errors.New("blueprint patch planning attempt not found")
	ErrBlueprintPatchPlanningAttemptConflict = errors.New("blueprint patch planning attempt conflict")
)

const blueprintPatchPlanningAttemptColumns = `
	workspace_id, build_run_id, revision_no, source_role,
	source_report_hash, source_run_id, output_text, output_hash,
	created_at, output_recorded_at`

func scanBlueprintPatchPlanningAttempt(row pgx.Row) (BlueprintPatchPlanningAttempt, error) {
	var attempt BlueprintPatchPlanningAttempt
	err := row.Scan(
		&attempt.WorkspaceID, &attempt.BuildRunID, &attempt.RevisionNo,
		&attempt.SourceRole, &attempt.SourceReportHash, &attempt.SourceRunID,
		&attempt.OutputText, &attempt.OutputHash, &attempt.CreatedAt,
		&attempt.OutputRecordedAt,
	)
	return attempt, err
}

// ReserveBlueprintPatchPlanningAttempt atomically binds the one permitted
// planner runtime run and its usage source before any graph work begins.
func (s *Store) ReserveBlueprintPatchPlanningAttempt(
	ctx context.Context,
	attempt BlueprintPatchPlanningAttempt,
) (BlueprintPatchPlanningAttempt, error) {
	if strings.TrimSpace(attempt.WorkspaceID) == "" || strings.TrimSpace(attempt.BuildRunID) == "" ||
		attempt.RevisionNo < 1 || strings.TrimSpace(attempt.SourceRole) == "" ||
		!canonicalSHA256Pattern.MatchString(attempt.SourceReportHash) || strings.TrimSpace(attempt.SourceRunID) == "" {
		return BlueprintPatchPlanningAttempt{}, errors.New("reserve blueprint patch planning attempt: invalid identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BlueprintPatchPlanningAttempt{}, fmt.Errorf("begin reserve blueprint patch planning attempt: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.clock.Now().UTC()
	recorded, err := scanBlueprintPatchPlanningAttempt(tx.QueryRow(ctx, `
		INSERT INTO weave_team_build_blueprint_patch_planning_attempts (
			workspace_id, build_run_id, revision_no, source_role,
			source_report_hash, source_run_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id, build_run_id, revision_no, source_role) DO NOTHING
		RETURNING `+blueprintPatchPlanningAttemptColumns,
		attempt.WorkspaceID, attempt.BuildRunID, attempt.RevisionNo, attempt.SourceRole,
		attempt.SourceReportHash, attempt.SourceRunID, now,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, readErr := scanBlueprintPatchPlanningAttempt(tx.QueryRow(ctx, `
			SELECT `+blueprintPatchPlanningAttemptColumns+`
			FROM weave_team_build_blueprint_patch_planning_attempts
			WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND source_role=$4
		`, attempt.WorkspaceID, attempt.BuildRunID, attempt.RevisionNo, attempt.SourceRole))
		if readErr != nil {
			return BlueprintPatchPlanningAttempt{}, fmt.Errorf("read existing blueprint patch planning attempt: %w", readErr)
		}
		if existing.SourceRunID != attempt.SourceRunID || existing.SourceReportHash != attempt.SourceReportHash {
			return BlueprintPatchPlanningAttempt{}, fmt.Errorf("%w: revision already has planner run %q", ErrBlueprintPatchPlanningAttemptConflict, existing.SourceRunID)
		}
		recorded = existing
	} else if err != nil {
		return BlueprintPatchPlanningAttempt{}, fmt.Errorf("reserve blueprint patch planning attempt: %w", err)
	}
	if _, err := s.RecordUsageSourceTx(ctx, tx, attempt.WorkspaceID, attempt.BuildRunID, BuildUsageSource{
		WorkspaceID: attempt.WorkspaceID, BuildRunID: attempt.BuildRunID,
		RoundNo: attempt.RevisionNo, SourceKind: UsageSourceKindBuildAgent,
		SourceRole: attempt.SourceRole, SourceRunID: attempt.SourceRunID,
	}); err != nil {
		return BlueprintPatchPlanningAttempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BlueprintPatchPlanningAttempt{}, fmt.Errorf("commit reserve blueprint patch planning attempt: %w", err)
	}
	return recorded, nil
}

func (s *Store) GetBlueprintPatchPlanningAttempt(
	ctx context.Context,
	workspaceID, buildRunID string,
	revisionNo int,
	sourceRole string,
) (BlueprintPatchPlanningAttempt, error) {
	attempt, err := scanBlueprintPatchPlanningAttempt(s.pool.QueryRow(ctx, `
		SELECT `+blueprintPatchPlanningAttemptColumns+`
		FROM weave_team_build_blueprint_patch_planning_attempts
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND source_role=$4
	`, workspaceID, buildRunID, revisionNo, sourceRole))
	if errors.Is(err, pgx.ErrNoRows) {
		return BlueprintPatchPlanningAttempt{}, ErrBlueprintPatchPlanningAttemptNotFound
	}
	if err != nil {
		return BlueprintPatchPlanningAttempt{}, fmt.Errorf("get blueprint patch planning attempt: %w", err)
	}
	return attempt, nil
}

// RecordBlueprintPatchPlanningOutput fills the attempt output exactly once.
// An exact replay is idempotent; a different output for the same attempt is a
// conflict and can never replace the recoverable value.
func (s *Store) RecordBlueprintPatchPlanningOutput(
	ctx context.Context,
	workspaceID, buildRunID string,
	revisionNo int,
	sourceRole, sourceRunID, output string,
) error {
	sum := sha256.Sum256([]byte(output))
	hash := hex.EncodeToString(sum[:])
	now := s.clock.Now().UTC()
	var storedOutput, storedHash string
	err := s.pool.QueryRow(ctx, `
		UPDATE weave_team_build_blueprint_patch_planning_attempts
		SET output_text=$6, output_hash=$7, output_recorded_at=$8
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND source_role=$4 AND source_run_id=$5 AND output_text IS NULL
		RETURNING output_text, output_hash
	`, workspaceID, buildRunID, revisionNo, sourceRole, sourceRunID, output, hash, now).
		Scan(&storedOutput, &storedHash)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("record blueprint patch planning output: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		SELECT output_text, output_hash
		FROM weave_team_build_blueprint_patch_planning_attempts
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND source_role=$4 AND source_run_id=$5
	`, workspaceID, buildRunID, revisionNo, sourceRole, sourceRunID).
		Scan(&storedOutput, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBlueprintPatchPlanningAttemptNotFound
	}
	if err != nil {
		return fmt.Errorf("read blueprint patch planning output: %w", err)
	}
	if storedOutput != output || storedHash != hash {
		return fmt.Errorf("%w: planner output already differs", ErrBlueprintPatchPlanningAttemptConflict)
	}
	return nil
}
