package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrCollaboratorConflict = errors.New("project collaborator already exists")

// Collaborator is one active or historical Team collaboration binding.
type Collaborator struct {
	ProjectID string     `json:"project_id"`
	TeamID    string     `json:"team_id"`
	AddedBy   string     `json:"added_by"`
	AddedAt   time.Time  `json:"added_at"`
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}

// ListCollaborators returns active collaboration bindings for one Project.
func (s *Store) ListCollaborators(ctx context.Context, workspaceID, projectID string) ([]Collaborator, error) {
	if _, err := s.Get(ctx, workspaceID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT project_id, team_id, added_by, added_at, removed_at
		FROM weave_project_collaborators
		WHERE workspace_id=$1 AND project_id=$2 AND removed_at IS NULL
		ORDER BY added_at DESC, team_id
	`, workspaceID, projectID)
	if err != nil {
		return nil, fmt.Errorf("list project collaborators: %w", err)
	}
	defer rows.Close()
	result := make([]Collaborator, 0)
	for rows.Next() {
		item, err := scanCollaborator(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project collaborator: %w", err)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// AddCollaborator adds or reactivates one Team collaborator for a Project.
func (s *Store) AddCollaborator(
	ctx context.Context,
	workspaceID, projectID, teamID, operatorID string,
) (Collaborator, error) {
	teamID = strings.TrimSpace(teamID)
	operatorID = strings.TrimSpace(operatorID)
	if teamID == "" {
		return Collaborator{}, ErrInvalidTeam
	}
	if operatorID == "" {
		return Collaborator{}, fmt.Errorf("add project collaborator: operator is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Collaborator{}, fmt.Errorf("begin add project collaborator: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ownerTeamID string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(team_id,'')
		FROM weave_projects
		WHERE workspace_id=$1 AND id=$2 AND archived_at IS NULL
		FOR SHARE
	`, workspaceID, projectID).Scan(&ownerTeamID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Collaborator{}, ErrNotFound
		}
		return Collaborator{}, fmt.Errorf("read project for collaborator: %w", err)
	}
	if ownerTeamID == teamID {
		return Collaborator{}, ErrCollaboratorConflict
	}
	var teamActive bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_teams AS team
			JOIN weave_agents AS lead
			  ON lead.workspace_id=team.workspace_id
			 AND lead.id=team.lead_avatar_id
			 AND lead.role='avatar'
			 AND lead.deleted=false
			WHERE team.workspace_id=$1 AND team.id=$2 AND team.status='active'
			FOR SHARE
		)
	`, workspaceID, teamID).Scan(&teamActive); err != nil {
		return Collaborator{}, fmt.Errorf("validate collaborator team: %w", err)
	}
	if !teamActive {
		return Collaborator{}, ErrInvalidTeam
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO weave_project_collaborators (
			workspace_id, project_id, team_id, added_by, added_at
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (workspace_id, project_id, team_id)
			WHERE removed_at IS NULL
		DO UPDATE SET added_at=weave_project_collaborators.added_at
		RETURNING project_id, team_id, added_by, added_at, removed_at
	`, workspaceID, projectID, teamID, operatorID, s.clock.Now())
	item, err := scanCollaborator(row)
	if err != nil {
		return Collaborator{}, fmt.Errorf("add project collaborator: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Collaborator{}, fmt.Errorf("commit add project collaborator: %w", err)
	}
	return item, nil
}

// RemoveCollaborator soft-deletes one active Project collaborator binding.
func (s *Store) RemoveCollaborator(ctx context.Context, workspaceID, projectID, teamID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE weave_project_collaborators
		SET removed_at=$4
		WHERE workspace_id=$1 AND project_id=$2 AND team_id=$3 AND removed_at IS NULL
	`, workspaceID, projectID, strings.TrimSpace(teamID), s.clock.Now())
	if err != nil {
		return fmt.Errorf("remove project collaborator: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, getErr := s.Get(ctx, workspaceID, projectID); getErr != nil {
			return getErr
		}
		return ErrNotFound
	}
	return nil
}

func scanCollaborator(row rowScanner) (Collaborator, error) {
	var item Collaborator
	err := row.Scan(&item.ProjectID, &item.TeamID, &item.AddedBy, &item.AddedAt, &item.RemovedAt)
	return item, err
}
