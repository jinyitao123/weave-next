package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type ResourceKind string

const (
	ResourceAttachment     ResourceKind = "attachment"
	ResourceMCPServer      ResourceKind = "mcp_server"
	ResourceLocalWorkspace ResourceKind = "local_workspace"
)

var (
	ErrInvalidResource  = errors.New("invalid project resource")
	ErrResourceConflict = errors.New("project resource is already bound")
	ErrResourceNotFound = errors.New("project resource not found")
)

type Resource struct {
	ID                string       `json:"id"`
	WorkspaceID       string       `json:"workspace_id"`
	ProjectID         string       `json:"project_id"`
	Kind              ResourceKind `json:"kind"`
	ResourceRef       string       `json:"resource_ref"`
	DisplayName       string       `json:"display_name"`
	RuntimeID         string       `json:"runtime_id,omitempty"`
	Available         bool         `json:"available"`
	UnavailableReason string       `json:"unavailable_reason,omitempty"`
	CreatedAt         time.Time    `json:"created_at"`
}

type CreateResourceInput struct {
	Kind        ResourceKind `json:"kind"`
	ResourceRef string       `json:"resource_ref"`
	DisplayName string       `json:"display_name"`
	RuntimeID   string       `json:"runtime_id,omitempty"`
}

func (s *Store) CreateResource(
	ctx context.Context, workspaceID, projectID string, input CreateResourceInput,
) (Resource, error) {
	input.ResourceRef = strings.TrimSpace(input.ResourceRef)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	input.RuntimeID = strings.TrimSpace(input.RuntimeID)
	if _, err := uuid.Parse(input.ResourceRef); err != nil || input.DisplayName == "" {
		return Resource{}, ErrInvalidResource
	}
	switch input.Kind {
	case ResourceAttachment, ResourceMCPServer:
		if input.RuntimeID != "" {
			return Resource{}, ErrInvalidResource
		}
	case ResourceLocalWorkspace:
		if _, err := uuid.Parse(input.RuntimeID); err != nil {
			return Resource{}, ErrInvalidResource
		}
	default:
		return Resource{}, ErrInvalidResource
	}
	if _, err := s.GetActive(ctx, workspaceID, projectID); err != nil {
		return Resource{}, err
	}
	resource, err := scanResource(s.pool.QueryRow(ctx, `
		INSERT INTO weave_project_resources (
			id,workspace_id,project_id,kind,resource_ref,display_name,runtime_id
		) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''))
		RETURNING id,workspace_id,project_id,kind,resource_ref,display_name,
			COALESCE(runtime_id,''),true,'',created_at
	`, uuid.NewString(), workspaceID, projectID, input.Kind, input.ResourceRef,
		input.DisplayName, input.RuntimeID))
	if err != nil {
		return Resource{}, mapResourceError(err)
	}
	return resource, nil
}

func (s *Store) ListResources(
	ctx context.Context, workspaceID, projectID string,
) ([]Resource, error) {
	if _, err := s.Get(ctx, workspaceID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT resource.id,resource.workspace_id,resource.project_id,resource.kind,
			resource.resource_ref,resource.display_name,COALESCE(resource.runtime_id,''),
			CASE resource.kind
			  WHEN 'attachment' THEN EXISTS (
			    SELECT 1 FROM weave_attachments source
			    WHERE source.workspace_id=resource.workspace_id AND source.id=resource.resource_ref)
			  WHEN 'mcp_server' THEN EXISTS (
			    SELECT 1 FROM weave_mcp_servers source
			    WHERE source.workspace_id=resource.workspace_id AND source.id=resource.resource_ref
			      AND source.enabled=true AND source.revoked_at IS NULL AND source.deleted_at IS NULL)
			  WHEN 'local_workspace' THEN EXISTS (
			    SELECT 1 FROM weave_runtimes source
			    WHERE source.workspace_id=resource.workspace_id AND source.id=resource.runtime_id
			      AND source.enabled=true AND source.revoked_at IS NULL AND source.deleted_at IS NULL)
			END AS available,
			CASE WHEN
			  CASE resource.kind
			    WHEN 'attachment' THEN EXISTS (SELECT 1 FROM weave_attachments source WHERE source.workspace_id=resource.workspace_id AND source.id=resource.resource_ref)
			    WHEN 'mcp_server' THEN EXISTS (SELECT 1 FROM weave_mcp_servers source WHERE source.workspace_id=resource.workspace_id AND source.id=resource.resource_ref AND source.enabled=true AND source.revoked_at IS NULL AND source.deleted_at IS NULL)
			    WHEN 'local_workspace' THEN EXISTS (SELECT 1 FROM weave_runtimes source WHERE source.workspace_id=resource.workspace_id AND source.id=resource.runtime_id AND source.enabled=true AND source.revoked_at IS NULL AND source.deleted_at IS NULL)
			  END THEN '' ELSE 'resource_unavailable' END,
			resource.created_at
		FROM weave_project_resources resource
		WHERE resource.workspace_id=$1 AND resource.project_id=$2
		ORDER BY resource.created_at DESC,resource.id
	`, workspaceID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project resources: %w", err)
	}
	defer rows.Close()
	resources := make([]Resource, 0)
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project resource: %w", err)
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list project resources: %w", err)
	}
	return resources, nil
}

func (s *Store) DeleteResource(
	ctx context.Context, workspaceID, projectID, resourceID string,
) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM weave_project_resources
		WHERE workspace_id=$1 AND project_id=$2 AND id=$3
	`, workspaceID, projectID, resourceID)
	if err != nil {
		return fmt.Errorf("delete project resource: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrResourceNotFound
	}
	return nil
}

func mapResourceError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("create project resource: %w", err)
	}
	switch pgErr.ConstraintName {
	case "weave_project_resources_identity_key":
		return ErrResourceConflict
	case "weave_project_resources_attachment_check",
		"weave_project_resources_mcp_server_check",
		"weave_project_resources_runtime_check",
		"weave_project_resources_shape_check":
		return ErrInvalidResource
	default:
		return fmt.Errorf("create project resource: %w", err)
	}
}

func scanResource(row rowScanner) (Resource, error) {
	var resource Resource
	err := row.Scan(
		&resource.ID, &resource.WorkspaceID, &resource.ProjectID, &resource.Kind,
		&resource.ResourceRef, &resource.DisplayName, &resource.RuntimeID,
		&resource.Available, &resource.UnavailableReason, &resource.CreatedAt,
	)
	return resource, err
}
