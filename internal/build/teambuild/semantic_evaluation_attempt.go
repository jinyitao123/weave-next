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
	ErrSemanticEvaluationAttemptNotFound = errors.New("semantic evaluation attempt not found")
	ErrSemanticEvaluationAttemptConflict = errors.New("semantic evaluation attempt conflict")
)

const semanticEvaluationAttemptColumns = `
	workspace_id, build_run_id, revision_no, source_role,
	evidence_hash, source_run_id, output_text, output_hash,
	created_at, output_recorded_at`

func scanSemanticEvaluationAttempt(row pgx.Row) (SemanticEvaluationAttempt, error) {
	var attempt SemanticEvaluationAttempt
	err := row.Scan(
		&attempt.WorkspaceID, &attempt.BuildRunID, &attempt.RevisionNo,
		&attempt.SourceRole, &attempt.EvidenceHash, &attempt.SourceRunID,
		&attempt.OutputText, &attempt.OutputHash, &attempt.CreatedAt,
		&attempt.OutputRecordedAt,
	)
	return attempt, err
}

// ReserveSemanticEvaluationAttempt binds the one evaluator run permitted for
// an immutable revision and records it as a recoverable budget usage source.
func (s *Store) ReserveSemanticEvaluationAttempt(
	ctx context.Context,
	attempt SemanticEvaluationAttempt,
) (SemanticEvaluationAttempt, error) {
	if strings.TrimSpace(attempt.WorkspaceID) == "" || strings.TrimSpace(attempt.BuildRunID) == "" ||
		attempt.RevisionNo < 1 || attempt.SourceRole != SourceRoleSemanticJudge ||
		!canonicalSHA256Pattern.MatchString(attempt.EvidenceHash) || strings.TrimSpace(attempt.SourceRunID) == "" {
		return SemanticEvaluationAttempt{}, errors.New("reserve semantic evaluation attempt: invalid identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SemanticEvaluationAttempt{}, fmt.Errorf("begin reserve semantic evaluation attempt: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.clock.Now().UTC()
	recorded, err := scanSemanticEvaluationAttempt(tx.QueryRow(ctx, `
		INSERT INTO weave_team_build_semantic_evaluation_attempts (
			workspace_id, build_run_id, revision_no, source_role,
			evidence_hash, source_run_id, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (workspace_id, build_run_id, revision_no, source_role) DO NOTHING
		RETURNING `+semanticEvaluationAttemptColumns,
		attempt.WorkspaceID, attempt.BuildRunID, attempt.RevisionNo,
		attempt.SourceRole, attempt.EvidenceHash, attempt.SourceRunID, now,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		existing, readErr := scanSemanticEvaluationAttempt(tx.QueryRow(ctx, `
			SELECT `+semanticEvaluationAttemptColumns+`
			FROM weave_team_build_semantic_evaluation_attempts
			WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND source_role=$4
		`, attempt.WorkspaceID, attempt.BuildRunID, attempt.RevisionNo, attempt.SourceRole))
		if readErr != nil {
			return SemanticEvaluationAttempt{}, fmt.Errorf("read existing semantic evaluation attempt: %w", readErr)
		}
		if existing.SourceRunID != attempt.SourceRunID || existing.EvidenceHash != attempt.EvidenceHash {
			return SemanticEvaluationAttempt{}, fmt.Errorf("%w: revision already has evaluator run %q", ErrSemanticEvaluationAttemptConflict, existing.SourceRunID)
		}
		recorded = existing
	} else if err != nil {
		return SemanticEvaluationAttempt{}, fmt.Errorf("reserve semantic evaluation attempt: %w", err)
	}
	if _, err := s.RecordUsageSourceTx(ctx, tx, attempt.WorkspaceID, attempt.BuildRunID, BuildUsageSource{
		WorkspaceID: attempt.WorkspaceID, BuildRunID: attempt.BuildRunID,
		RoundNo: attempt.RevisionNo, SourceKind: UsageSourceKindBuildAgent,
		SourceRole: attempt.SourceRole, SourceRunID: attempt.SourceRunID,
	}); err != nil {
		return SemanticEvaluationAttempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return SemanticEvaluationAttempt{}, fmt.Errorf("commit reserve semantic evaluation attempt: %w", err)
	}
	return recorded, nil
}

func (s *Store) GetSemanticEvaluationAttempt(
	ctx context.Context,
	workspaceID, buildRunID string,
	revisionNo int,
	sourceRole string,
) (SemanticEvaluationAttempt, error) {
	attempt, err := scanSemanticEvaluationAttempt(s.pool.QueryRow(ctx, `
		SELECT `+semanticEvaluationAttemptColumns+`
		FROM weave_team_build_semantic_evaluation_attempts
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3 AND source_role=$4
	`, workspaceID, buildRunID, revisionNo, sourceRole))
	if errors.Is(err, pgx.ErrNoRows) {
		return SemanticEvaluationAttempt{}, ErrSemanticEvaluationAttemptNotFound
	}
	if err != nil {
		return SemanticEvaluationAttempt{}, fmt.Errorf("get semantic evaluation attempt: %w", err)
	}
	return attempt, nil
}

// RecordSemanticEvaluationOutput fills the evaluator output exactly once.
// Exact replay is idempotent; different output for the same run is rejected.
func (s *Store) RecordSemanticEvaluationOutput(
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
		UPDATE weave_team_build_semantic_evaluation_attempts
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
		return fmt.Errorf("record semantic evaluation output: %w", err)
	}
	err = s.pool.QueryRow(ctx, `
		SELECT output_text, output_hash
		FROM weave_team_build_semantic_evaluation_attempts
		WHERE workspace_id=$1 AND build_run_id=$2 AND revision_no=$3
		  AND source_role=$4 AND source_run_id=$5
	`, workspaceID, buildRunID, revisionNo, sourceRole, sourceRunID).
		Scan(&storedOutput, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSemanticEvaluationAttemptNotFound
	}
	if err != nil {
		return fmt.Errorf("read semantic evaluation output: %w", err)
	}
	if storedOutput != output || storedHash != hash {
		return fmt.Errorf("%w: evaluator output already differs", ErrSemanticEvaluationAttemptConflict)
	}
	return nil
}
