package workflow

import (
	"context"
	"errors"
	"fmt"

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

// GetArtifact returns immutable artifact content for one exact publication.
func (s *ArtifactStore) GetArtifact(
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
func (s *ArtifactStore) ListDependencies(
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
