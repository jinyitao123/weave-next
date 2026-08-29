package workflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

const artifactColumns = `
	workspace_id, workflow_id, workflow_version, artifact_schema_version,
	canonicalization_algorithm, canonicalization_version, hash_algorithm,
	content_hash, payload, created_at
`

const dependencyColumns = `
	workspace_id, workflow_id, workflow_version, owner_type, owner_id,
	owner_agent_version, dependency_type, dependency_key, dependency_version,
	content_hash
`

// InsertPublicationTx inserts every publication fact into the caller-owned
// transaction. The caller alone is responsible for commit or rollback.
func (s *Store) InsertPublicationTx(
	ctx context.Context,
	tx pgx.Tx,
	publication Publication,
) error {
	if tx == nil {
		return errors.New("insert workflow publication: transaction is required")
	}
	if err := validatePublicationIdentity(publication); err != nil {
		return err
	}

	var (
		workflowStatus    string
		workflowUpdatedAt time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT status, updated_at
		FROM weave_team_workflows
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, publication.WorkspaceID, publication.WorkflowID).Scan(
		&workflowStatus,
		&workflowUpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: workflow %q", ErrNotFound, publication.WorkflowID)
	}
	if err != nil {
		return fmt.Errorf("lock workflow for publication: %w", err)
	}
	if workflowStatus == WorkflowStatusArchived {
		return fmt.Errorf("%w: workflow %q", ErrArchived, publication.WorkflowID)
	}

	var (
		version          int
		versionUpdatedAt time.Time
	)
	err = tx.QueryRow(ctx, `
		SELECT version, updated_at
		FROM weave_team_workflow_versions
		WHERE workspace_id=$1
		  AND workflow_id=$2
		  AND version=$3
		  AND status='draft'
		  AND updated_at=$4
		FOR UPDATE
	`,
		publication.WorkspaceID,
		publication.WorkflowID,
		publication.WorkflowVersion,
		publication.ExpectedUpdatedAt,
	).Scan(&version, &versionUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf(
			"%w: workflow %q version %d",
			ErrVersionConflict,
			publication.WorkflowID,
			publication.WorkflowVersion,
		)
	}
	if err != nil {
		return fmt.Errorf("lock workflow version for publication: %w", err)
	}
	effectivePublishedAt := effectivePublicationTime(
		s.clock.Now(),
		workflowUpdatedAt,
		versionUpdatedAt,
	)

	dependencies := append([]TeamWorkflowDependency(nil), publication.Dependencies...)
	sort.Slice(dependencies, func(i, j int) bool {
		return dependencyLess(dependencies[i], dependencies[j])
	})
	for _, dependency := range dependencies {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_team_workflow_dependencies (
				workspace_id, workflow_id, workflow_version, owner_type, owner_id,
				owner_agent_version, dependency_type, dependency_key,
				dependency_version, content_hash
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		`,
			publication.WorkspaceID,
			publication.WorkflowID,
			publication.WorkflowVersion,
			dependency.OwnerType,
			dependency.OwnerID,
			dependency.OwnerAgentVersion,
			dependency.DependencyType,
			dependency.DependencyKey,
			dependency.DependencyVersion,
			dependency.ContentHash,
		); err != nil {
			return fmt.Errorf("insert workflow publication dependency: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE weave_team_workflow_versions
		SET status='published',
			published_at=$4,
			updated_at=$4
		WHERE workspace_id=$1 AND workflow_id=$2 AND version=$3
	`,
		publication.WorkspaceID,
		publication.WorkflowID,
		publication.WorkflowVersion,
		effectivePublishedAt,
	); err != nil {
		return fmt.Errorf("publish workflow version: %w", err)
	}

	artifact := publication.Artifact
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_published_artifact_contents (
			workspace_id, workflow_id, workflow_version, artifact_schema_version,
			canonicalization_algorithm, canonicalization_version, hash_algorithm,
			content_hash, payload, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		publication.WorkspaceID,
		publication.WorkflowID,
		publication.WorkflowVersion,
		artifact.ArtifactSchemaVersion,
		artifact.CanonicalizationAlgorithm,
		artifact.CanonicalizationVersion,
		artifact.HashAlgorithm,
		artifact.ContentHash,
		artifact.Payload,
		effectivePublishedAt,
	); err != nil {
		return fmt.Errorf("insert published artifact content: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_workflow_version_admission_statuses (
			workspace_id, workflow_id, workflow_version, blocked
		) VALUES ($1, $2, $3, false)
	`,
		publication.WorkspaceID,
		publication.WorkflowID,
		publication.WorkflowVersion,
	); err != nil {
		return fmt.Errorf("insert workflow version admission status: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE weave_team_workflows
		SET published_version=$3,
			updated_at=$4
		WHERE workspace_id=$1 AND id=$2
	`,
		publication.WorkspaceID,
		publication.WorkflowID,
		publication.WorkflowVersion,
		effectivePublishedAt,
	); err != nil {
		return fmt.Errorf("set workflow published version: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_teams AS team
		SET default_workflow_id=$2, updated_at=GREATEST(team.updated_at,$3)
		FROM weave_team_workflows AS published
		WHERE published.workspace_id=$1 AND published.id=$2
		  AND team.workspace_id=published.workspace_id
		  AND team.id=published.team_id
		  AND team.default_workflow_id IS NULL
	`, publication.WorkspaceID, publication.WorkflowID, effectivePublishedAt); err != nil {
		return fmt.Errorf("set team default workflow: %w", err)
	}
	return nil
}

func effectivePublicationTime(
	clockNow, workflowUpdatedAt, versionUpdatedAt time.Time,
) time.Time {
	effective := clockNow.Truncate(time.Microsecond)
	for _, updatedAt := range []time.Time{workflowUpdatedAt, versionUpdatedAt} {
		candidate := updatedAt.Truncate(time.Microsecond).Add(time.Microsecond)
		if candidate.After(effective) {
			effective = candidate
		}
	}
	return effective
}

// GetArtifact returns immutable artifact content for one exact publication.
func (s *Store) GetArtifact(
	ctx context.Context,
	workspaceID, workflowID string,
	version int,
) (*PublishedArtifactContent, error) {
	artifact, err := scanArtifact(s.pool.QueryRow(ctx, `
		SELECT `+artifactColumns+`
		FROM weave_published_artifact_contents
		WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
	`, workspaceID, workflowID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf(
			"%w: workflow %q version %d artifact",
			ErrNotFound,
			workflowID,
			version,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("get published artifact content: %w", err)
	}
	return artifact, nil
}

// ListDependencies returns an exact publication's dependency index in
// canonical stable order.
func (s *Store) ListDependencies(
	ctx context.Context,
	workspaceID, workflowID string,
	version int,
) ([]TeamWorkflowDependency, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+dependencyColumns+`
		FROM weave_team_workflow_dependencies
		WHERE workspace_id=$1 AND workflow_id=$2 AND workflow_version=$3
		ORDER BY
			workspace_id COLLATE "C",
			owner_type COLLATE "C",
			owner_id COLLATE "C",
			owner_agent_version NULLS FIRST,
			dependency_type COLLATE "C",
			dependency_key COLLATE "C",
			dependency_version NULLS FIRST,
			content_hash COLLATE "C"
	`, workspaceID, workflowID, version)
	if err != nil {
		return nil, fmt.Errorf("list workflow publication dependencies: %w", err)
	}
	defer rows.Close()

	dependencies := make([]TeamWorkflowDependency, 0)
	for rows.Next() {
		dependency, err := scanDependency(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow publication dependency: %w", err)
		}
		dependencies = append(dependencies, *dependency)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workflow publication dependencies: %w", err)
	}
	return dependencies, nil
}

func validatePublicationIdentity(publication Publication) error {
	artifact := publication.Artifact
	if artifact.WorkspaceID != publication.WorkspaceID ||
		artifact.WorkflowID != publication.WorkflowID ||
		artifact.WorkflowVersion != publication.WorkflowVersion {
		return errors.New("insert workflow publication: artifact identity does not match publication")
	}
	for i, dependency := range publication.Dependencies {
		if dependency.WorkspaceID != publication.WorkspaceID ||
			dependency.WorkflowID != publication.WorkflowID ||
			dependency.WorkflowVersion != publication.WorkflowVersion {
			return fmt.Errorf(
				"insert workflow publication: dependency %d identity does not match publication",
				i,
			)
		}
	}
	return nil
}

func dependencyLess(left, right TeamWorkflowDependency) bool {
	if left.WorkspaceID != right.WorkspaceID {
		return left.WorkspaceID < right.WorkspaceID
	}
	if left.OwnerType != right.OwnerType {
		return left.OwnerType < right.OwnerType
	}
	if left.OwnerID != right.OwnerID {
		return left.OwnerID < right.OwnerID
	}
	if comparison := compareDependencyOptionalInt64(left.OwnerAgentVersion, right.OwnerAgentVersion); comparison != 0 {
		return comparison < 0
	}
	if left.DependencyType != right.DependencyType {
		return left.DependencyType < right.DependencyType
	}
	if left.DependencyKey != right.DependencyKey {
		return left.DependencyKey < right.DependencyKey
	}
	if comparison := compareDependencyOptionalInt64(left.DependencyVersion, right.DependencyVersion); comparison != 0 {
		return comparison < 0
	}
	return left.ContentHash < right.ContentHash
}

func compareDependencyOptionalInt64(left, right *int64) int {
	switch {
	case left == nil && right == nil:
		return 0
	case left == nil:
		return -1
	case right == nil:
		return 1
	case *left < *right:
		return -1
	case *left > *right:
		return 1
	default:
		return 0
	}
}

func scanArtifact(row rowScanner) (*PublishedArtifactContent, error) {
	var artifact PublishedArtifactContent
	var payload []byte
	err := row.Scan(
		&artifact.WorkspaceID,
		&artifact.WorkflowID,
		&artifact.WorkflowVersion,
		&artifact.ArtifactSchemaVersion,
		&artifact.CanonicalizationAlgorithm,
		&artifact.CanonicalizationVersion,
		&artifact.HashAlgorithm,
		&artifact.ContentHash,
		&payload,
		&artifact.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	artifact.Payload = append(artifact.Payload, payload...)
	return &artifact, nil
}

func scanDependency(row rowScanner) (*TeamWorkflowDependency, error) {
	var dependency TeamWorkflowDependency
	err := row.Scan(
		&dependency.WorkspaceID,
		&dependency.WorkflowID,
		&dependency.WorkflowVersion,
		&dependency.OwnerType,
		&dependency.OwnerID,
		&dependency.OwnerAgentVersion,
		&dependency.DependencyType,
		&dependency.DependencyKey,
		&dependency.DependencyVersion,
		&dependency.ContentHash,
	)
	if err != nil {
		return nil, err
	}
	return &dependency, nil
}
