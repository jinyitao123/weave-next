// Package projects resolves workspace-scoped Projects for task execution.
package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound      = errors.New("project not found")
	ErrInvalidName   = errors.New("project name is required")
	ErrInvalidAvatar = errors.New("project avatar must reference an active avatar")
	ErrInvalidTeam   = errors.New("project team must reference an active team")
	ErrNameConflict  = errors.New("active project name already exists for avatar")
	ErrArchived      = errors.New("project is archived")
)

// Clock supplies timestamps for Project mutations.
type Clock interface {
	Now() time.Time
}

// RealClock uses the process wall clock.
type RealClock struct{}

// Now returns the current time.
func (RealClock) Now() time.Time { return time.Now() }

// Project is one workspace-scoped body of work owned by an Avatar.
type Project struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	TeamID         string     `json:"team_id"`
	AvatarID       string     `json:"avatar_id"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	ArchivedAt     *time.Time `json:"archived_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	LastActivityAt *time.Time `json:"last_activity_at"`
	// SystemKind marks platform-owned Projects (e.g. "unclassified");
	// user-created Projects have "" (DB NULL normalized to empty string).
	SystemKind string `json:"system_kind"`
}

// Store resolves Project ownership and ensures the default execution Project.
type Store struct {
	pool  *pgxpool.Pool
	clock Clock
}

// New creates a Project store.
func New(pool *pgxpool.Pool, clock Clock) *Store {
	if clock == nil {
		clock = RealClock{}
	}
	return &Store{pool: pool, clock: clock}
}

// Get returns one Project, including archived Projects.
func (s *Store) Get(ctx context.Context, workspaceID, id string) (Project, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+projectColumns+`
		FROM weave_projects
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id)
	project, err := scanProject(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("get project: %w", err)
	}
	return project, nil
}

// GetActive returns one Project only when it can accept new work.
func (s *Store) GetActive(ctx context.Context, workspaceID, id string) (Project, error) {
	project, err := s.Get(ctx, workspaceID, id)
	if err != nil {
		return Project{}, err
	}
	if project.ArchivedAt != nil {
		return Project{}, ErrArchived
	}
	return project, nil
}

// EnsureUnclassified returns the active compatibility Project for an Avatar.
func (s *Store) EnsureUnclassified(
	ctx context.Context,
	workspaceID, avatarID string,
) (Project, error) {
	return s.ensureUnclassified(ctx, workspaceID, "", avatarID)
}

func (s *Store) ensureUnclassified(
	ctx context.Context,
	workspaceID, teamID, avatarID string,
) (Project, error) {
	teamID, avatarID, err := s.resolveProjectOwner(ctx, workspaceID, teamID, avatarID)
	if err != nil {
		return Project{}, err
	}
	id := uuid.NewString()
	now := s.clock.Now()
	row := s.pool.QueryRow(ctx, `
		INSERT INTO weave_projects (
			id, workspace_id, team_id, avatar_id, name, description, system_kind,
			created_at, updated_at
		)
		SELECT $1, team.workspace_id, team.id, team.lead_avatar_id,
			CASE WHEN EXISTS (
				SELECT 1 FROM weave_projects AS named
				WHERE named.workspace_id=team.workspace_id
				  AND named.team_id=team.id
				  AND named.archived_at IS NULL
				  AND lower(named.name)=lower('未分类')
			) THEN '未分类 ' || left($1, 8) ELSE '未分类' END,
			'', 'unclassified', $5, $5
		FROM weave_teams AS team
		WHERE team.workspace_id=$2 AND team.id=$3
		  AND team.lead_avatar_id=$4 AND team.status='active'
		ON CONFLICT (workspace_id, team_id)
			WHERE system_kind = 'unclassified' AND archived_at IS NULL
		DO UPDATE SET updated_at=weave_projects.updated_at
		RETURNING `+projectColumns,
		id, workspaceID, teamID, avatarID, now,
	)
	project, err := scanProject(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrInvalidTeam
	}
	if err != nil {
		return Project{}, classifyMutationError(err)
	}
	if project.ArchivedAt != nil {
		return Project{}, ErrArchived
	}
	return project, nil
}

func (s *Store) resolveProjectOwner(ctx context.Context, workspaceID, teamID, avatarID string) (string, string, error) {
	teamID = strings.TrimSpace(teamID)
	avatarID = strings.TrimSpace(avatarID)
	if teamID != "" {
		var lead string
		err := s.pool.QueryRow(ctx, `
			SELECT team.lead_avatar_id
			FROM weave_teams AS team
			JOIN weave_agents AS lead
			  ON lead.workspace_id=team.workspace_id
			 AND lead.id=team.lead_avatar_id
			 AND lead.role='avatar'
			 AND lead.deleted=false
			WHERE team.workspace_id=$1 AND team.id=$2 AND team.status='active'
		`, workspaceID, teamID).Scan(&lead)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", ErrInvalidTeam
		}
		if err != nil {
			return "", "", fmt.Errorf("resolve project team: %w", err)
		}
		return teamID, lead, nil
	}
	if avatarID == "" {
		return "", "", ErrInvalidTeam
	}
	err := s.pool.QueryRow(ctx, `
		SELECT team.id, team.lead_avatar_id
		FROM weave_teams AS team
		JOIN weave_agents AS lead
		  ON lead.workspace_id=team.workspace_id
		 AND lead.id=team.lead_avatar_id
		 AND lead.role='avatar'
		 AND lead.deleted=false
		WHERE team.workspace_id=$1 AND team.lead_avatar_id=$2 AND team.status='active'
	`, workspaceID, avatarID).Scan(&teamID, &avatarID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrInvalidAvatar
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve legacy project avatar: %w", err)
	}
	return teamID, avatarID, nil
}

func classifyMutationError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.ConstraintName {
	case "weave_projects_active_name_key", "weave_projects_active_team_name_key":
		return ErrNameConflict
	case "weave_projects_avatar_fk", "weave_projects_avatar_role_check":
		return ErrInvalidAvatar
	case "weave_projects_team_fk", "weave_projects_team_required_check":
		return ErrInvalidTeam
	case "weave_projects_name_check":
		return ErrInvalidName
	default:
		return err
	}
}

const projectColumns = `
	id, workspace_id, COALESCE(team_id,''), avatar_id, name, description,
	COALESCE(system_kind,''), archived_at, created_at, updated_at, last_activity_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProject(row rowScanner) (Project, error) {
	var project Project
	err := row.Scan(
		&project.ID,
		&project.WorkspaceID,
		&project.TeamID,
		&project.AvatarID,
		&project.Name,
		&project.Description,
		&project.SystemKind,
		&project.ArchivedAt,
		&project.CreatedAt,
		&project.UpdatedAt,
		&project.LastActivityAt,
	)
	return project, err
}
