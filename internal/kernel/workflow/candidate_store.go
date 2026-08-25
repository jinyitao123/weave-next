package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jinyitao123/weave/internal/base/frozen"
)

var (
	// ErrCandidateNotFound indicates that no frozen candidate matches the
	// requested workflow identity and content hash.
	ErrCandidateNotFound = errors.New("workflow candidate not found")
	// ErrCandidateInvalid indicates that a persisted candidate row is
	// structurally corrupt or does not match its frozen envelope.
	ErrCandidateInvalid = errors.New("workflow candidate is invalid")
)

// InsertCandidateTx persists one frozen publication candidate immutably in
// the caller-owned transaction. The caller alone decides when the candidate
// row becomes visible (commit) and supplies the writing principal.
func (s *Store) InsertCandidateTx(
	ctx context.Context,
	tx pgx.Tx,
	candidate *PublicationCandidate,
	createdBy string,
) error {
	if tx == nil {
		return errors.New("insert workflow candidate: transaction is required")
	}
	if candidate == nil {
		return fmt.Errorf("insert workflow candidate: %w", ErrCandidateInvalid)
	}
	createdBy = strings.TrimSpace(createdBy)
	if createdBy == "" {
		return errors.New("insert workflow candidate: created_by is required")
	}
	envelope, err := candidateEnvelope(candidate)
	if err != nil {
		return err
	}
	if _, err := frozen.DecodeArtifactEnvelopeV1(envelope); err != nil {
		return fmt.Errorf("insert workflow candidate: %w", ErrCandidateInvalid)
	}
	envelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("insert workflow candidate: encode envelope: %w", err)
	}
	dependenciesJSON, err := json.Marshal(candidate.Dependencies)
	if err != nil {
		return fmt.Errorf("insert workflow candidate: encode dependencies: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_workflow_candidates (
			workspace_id, workflow_id, workflow_version, content_hash,
			envelope_json, dependencies_json, expected_updated_at,
			created_by, created_at
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9)
	`,
		candidate.WorkspaceID,
		candidate.WorkflowID,
		candidate.WorkflowVersion,
		candidate.ContentHash,
		envelopeJSON,
		dependenciesJSON,
		candidate.ExpectedUpdatedAt,
		createdBy,
		s.clock.Now().UTC(),
	); err != nil {
		return fmt.Errorf("insert workflow candidate: %w", err)
	}
	return nil
}

// GetCandidateTx returns one frozen candidate by workspace, workflow, and
// content hash. The content hash binds the workflow version, so the request
// never needs to carry a version.
func (s *Store) GetCandidateTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, workflowID, contentHash string,
) (*PublicationCandidate, error) {
	if tx == nil {
		return nil, errors.New("get workflow candidate: transaction is required")
	}
	var (
		candidate      PublicationCandidate
		envelopeJSON   []byte
		dependencies   []byte
		expectedUpdate time.Time
		createdBy      string
		createdAt      time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT workspace_id, workflow_id, workflow_version, content_hash,
			envelope_json, dependencies_json, expected_updated_at,
			created_by, created_at
		FROM weave_team_workflow_candidates
		WHERE workspace_id=$1 AND workflow_id=$2 AND content_hash=$3
	`, workspaceID, workflowID, contentHash).Scan(
		&candidate.WorkspaceID,
		&candidate.WorkflowID,
		&candidate.WorkflowVersion,
		&candidate.ContentHash,
		&envelopeJSON,
		&dependencies,
		&expectedUpdate,
		&createdBy,
		&createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"%w: workflow %q candidate %s",
			ErrCandidateNotFound, workflowID, contentHash,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow candidate: %w", err)
	}
	var envelope frozen.ArtifactEnvelopeV1
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		return nil, fmt.Errorf("get workflow candidate: %w", ErrCandidateInvalid)
	}
	if envelope.WorkspaceID != candidate.WorkspaceID ||
		envelope.WorkflowID != candidate.WorkflowID ||
		envelope.WorkflowVersion != candidate.WorkflowVersion ||
		envelope.ContentHash != candidate.ContentHash {
		return nil, fmt.Errorf("get workflow candidate: %w", ErrCandidateInvalid)
	}
	payload, err := frozen.DecodeArtifactEnvelopeV1(envelope)
	if err != nil {
		return nil, fmt.Errorf("get workflow candidate: %w", ErrCandidateInvalid)
	}
	if err := json.Unmarshal(dependencies, &candidate.Dependencies); err != nil {
		return nil, fmt.Errorf("get workflow candidate: %w", ErrCandidateInvalid)
	}
	candidate.ExpectedUpdatedAt = expectedUpdate
	candidate.Payload = payload
	candidate.ArtifactSchemaVersion = envelope.ArtifactSchemaVersion
	candidate.CanonicalizationAlgorithm = envelope.CanonicalizationAlgorithm
	candidate.CanonicalizationVersion = envelope.CanonicalizationVersion
	candidate.HashAlgorithm = envelope.HashAlgorithm
	return &candidate, nil
}

// GetCandidateArtifact reads one frozen candidate envelope by content hash
// and returns it in the same shape the published artifact reader returns.
// It is the candidate-backed ArtifactReader implementation used by the
// teamrun executor for candidate test runs.
func (s *Store) GetCandidateArtifact(
	ctx context.Context,
	workspaceID, workflowID string,
	workflowVersion int,
	contentHash string,
) (*PublishedArtifactContent, error) {
	var (
		artifact     PublishedArtifactContent
		envelopeJSON []byte
		createdAt    time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT workspace_id, workflow_id, workflow_version, content_hash,
			envelope_json, created_at
		FROM weave_team_workflow_candidates
		WHERE workspace_id=$1 AND workflow_id=$2
			AND workflow_version=$3 AND content_hash=$4
	`, workspaceID, workflowID, workflowVersion, contentHash).Scan(
		&artifact.WorkspaceID,
		&artifact.WorkflowID,
		&artifact.WorkflowVersion,
		&artifact.ContentHash,
		&envelopeJSON,
		&createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"%w: workflow %q version %d candidate %s",
			ErrCandidateNotFound, workflowID, workflowVersion, contentHash,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow candidate artifact: %w", err)
	}
	var envelope frozen.ArtifactEnvelopeV1
	if err := json.Unmarshal(envelopeJSON, &envelope); err != nil {
		return nil, fmt.Errorf("get workflow candidate artifact: %w", ErrCandidateInvalid)
	}
	if _, err := frozen.DecodeArtifactEnvelopeV1(envelope); err != nil {
		return nil, fmt.Errorf("get workflow candidate artifact: %w", ErrCandidateInvalid)
	}
	artifact.ArtifactSchemaVersion = envelope.ArtifactSchemaVersion
	artifact.CanonicalizationAlgorithm = envelope.CanonicalizationAlgorithm
	artifact.CanonicalizationVersion = envelope.CanonicalizationVersion
	artifact.HashAlgorithm = envelope.HashAlgorithm
	artifact.Payload = append(json.RawMessage(nil), envelope.Payload...)
	artifact.CreatedAt = createdAt
	return &artifact, nil
}

func candidateEnvelope(candidate *PublicationCandidate) (frozen.ArtifactEnvelopeV1, error) {
	payloadBytes, err := frozen.Canonicalize(candidate.Payload, frozen.PreorderArtifactPayloadV1)
	if err != nil {
		return frozen.ArtifactEnvelopeV1{}, fmt.Errorf(
			"insert workflow candidate: canonicalize payload: %w", err,
		)
	}
	return frozen.ArtifactEnvelopeV1{
		WorkspaceID:               candidate.WorkspaceID,
		WorkflowID:                candidate.WorkflowID,
		WorkflowVersion:           candidate.WorkflowVersion,
		ArtifactSchemaVersion:     candidate.ArtifactSchemaVersion,
		CanonicalizationAlgorithm: candidate.CanonicalizationAlgorithm,
		CanonicalizationVersion:   candidate.CanonicalizationVersion,
		HashAlgorithm:             candidate.HashAlgorithm,
		ContentHash:               candidate.ContentHash,
		Payload:                   payloadBytes,
	}, nil
}
