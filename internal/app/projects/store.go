// Package projects persists workspace-scoped Project catalogs.
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

// ListFilter narrows one workspace's Project catalog.
type ListFilter struct {
	AvatarID        string
	TeamID          string
	IncludeArchived bool
}

// Store persists Projects and their ownership transfers.
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

// Create adds an active Project owned by one Avatar.
func (s *Store) Create(
	ctx context.Context,
	workspaceID, avatarID, name, description string,
) (Project, error) {
	return s.create(ctx, workspaceID, "", avatarID, name, description)
}

// CreateForTeam adds an active Project owned by one Team. avatar_id is a
// compatibility mirror derived from the team's current lead.
func (s *Store) CreateForTeam(
	ctx context.Context,
	workspaceID, teamID, name, description string,
) (Project, error) {
	return s.create(ctx, workspaceID, teamID, "", name, description)
}

func (s *Store) create(
	ctx context.Context,
	workspaceID, teamID, avatarID, name, description string,
) (Project, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Project{}, err
	}
	teamID, avatarID, err = s.resolveProjectOwner(ctx, workspaceID, teamID, avatarID)
	if err != nil {
		return Project{}, err
	}
	now := s.clock.Now()
	row := s.pool.QueryRow(ctx, `
		INSERT INTO weave_projects (
			id, workspace_id, team_id, avatar_id, name, description, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		RETURNING `+projectColumns,
		uuid.NewString(), workspaceID, teamID, avatarID, name,
		strings.TrimSpace(description), now,
	)
	project, scanErr := scanProject(row)
	if scanErr != nil {
		return Project{}, classifyMutationError(scanErr)
	}
	return project, nil
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

// EnsureUnclassifiedForTeam returns the active compatibility Project for a
// Team. avatar_id is mirrored from the current lead.
func (s *Store) EnsureUnclassifiedForTeam(
	ctx context.Context,
	workspaceID, teamID string,
) (Project, error) {
	return s.ensureUnclassified(ctx, workspaceID, teamID, "")
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

// List returns Projects from one workspace in stable product order.
func (s *Store) List(ctx context.Context, workspaceID string, filter ListFilter) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+projectColumns+`
		FROM weave_projects
		WHERE workspace_id=$1
		  AND ($2='' OR avatar_id=$2)
		  AND ($3='' OR team_id=$3)
		  AND ($4 OR archived_at IS NULL)
		ORDER BY (archived_at IS NOT NULL), COALESCE(last_activity_at, updated_at) DESC, id
	`, workspaceID, strings.TrimSpace(filter.AvatarID), strings.TrimSpace(filter.TeamID), filter.IncludeArchived)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	result := make([]Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project list: %w", err)
		}
		result = append(result, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return result, nil
}

// Update changes user-editable Project metadata.
func (s *Store) Update(
	ctx context.Context,
	workspaceID, id, name, description string,
) (Project, error) {
	name, err := normalizeName(name)
	if err != nil {
		return Project{}, err
	}
	row := s.pool.QueryRow(ctx, `
		UPDATE weave_projects
		SET name=$3, description=$4, updated_at=$5
		WHERE workspace_id=$1 AND id=$2
		RETURNING `+projectColumns,
		workspaceID, id, name, strings.TrimSpace(description), s.clock.Now(),
	)
	project, scanErr := scanProject(row)
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if scanErr != nil {
		return Project{}, classifyMutationError(scanErr)
	}
	return project, nil
}

// Archive idempotently prevents a Project from accepting new work.
func (s *Store) Archive(ctx context.Context, workspaceID, id string) (Project, error) {
	now := s.clock.Now()
	row := s.pool.QueryRow(ctx, `
		UPDATE weave_projects
		SET archived_at=COALESCE(archived_at, $3),
		    updated_at=CASE WHEN archived_at IS NULL THEN $3 ELSE updated_at END
		WHERE workspace_id=$1 AND id=$2
		RETURNING `+projectColumns,
		workspaceID, id, now,
	)
	project, err := scanProject(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("archive project: %w", err)
	}
	return project, nil
}

// Restore idempotently reactivates an archived Project.
func (s *Store) Restore(ctx context.Context, workspaceID, id string) (Project, error) {
	now := s.clock.Now()
	row := s.pool.QueryRow(ctx, `
		UPDATE weave_projects
		SET archived_at=NULL,
		    updated_at=CASE WHEN archived_at IS NOT NULL THEN $3 ELSE updated_at END
		WHERE workspace_id=$1 AND id=$2
		RETURNING `+projectColumns,
		workspaceID, id, now,
	)
	project, scanErr := scanProject(row)
	if errors.Is(scanErr, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if scanErr != nil {
		return Project{}, classifyMutationError(scanErr)
	}
	return project, nil
}

// Move transfers current ownership to another Avatar and appends an audit row.
func (s *Store) Move(
	ctx context.Context,
	workspaceID, id, avatarID, operatorID string,
) (Project, error) {
	return s.move(ctx, workspaceID, id, "", avatarID, operatorID)
}

// MoveForTeam transfers current ownership to another Team and appends an audit
// row using the mirrored lead avatar IDs.
func (s *Store) MoveForTeam(
	ctx context.Context,
	workspaceID, id, teamID, operatorID string,
) (Project, error) {
	return s.move(ctx, workspaceID, id, teamID, "", operatorID)
}

func (s *Store) move(
	ctx context.Context,
	workspaceID, id, teamID, avatarID, operatorID string,
) (Project, error) {
	var err error
	teamID, avatarID, err = s.resolveProjectOwner(ctx, workspaceID, teamID, avatarID)
	if err != nil {
		return Project{}, err
	}
	operatorID = strings.TrimSpace(operatorID)
	if operatorID == "" {
		return Project{}, fmt.Errorf("move project: operator is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Project{}, fmt.Errorf("begin move project: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, err := scanProject(tx.QueryRow(ctx, `
		SELECT `+projectColumns+`
		FROM weave_projects
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, fmt.Errorf("lock project for move: %w", err)
	}
	if current.TeamID == teamID {
		return current, nil
	}

	now := s.clock.Now()
	moved, err := scanProject(tx.QueryRow(ctx, `
		UPDATE weave_projects
		SET team_id=$3, avatar_id=$4, system_kind=NULL, updated_at=$5
		WHERE workspace_id=$1 AND id=$2
		RETURNING `+projectColumns,
		workspaceID, id, teamID, avatarID, now,
	))
	if err != nil {
		return Project{}, classifyMutationError(err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_project_move_audits (
			id, workspace_id, project_id, from_avatar_id, to_avatar_id,
			operator_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, uuid.NewString(), workspaceID, id, current.AvatarID, avatarID, operatorID, now); err != nil {
		return Project{}, fmt.Errorf("audit project move: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit project move: %w", err)
	}
	return moved, nil
}

// ListUnresolvedActive returns active legacy Projects that still lack a
// canonical Team owner. It backs the operator clear-down query exposed in the
// T2 handoff.
func (s *Store) ListUnresolvedActive(ctx context.Context, workspaceID string) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+projectColumns+`
		FROM weave_projects
		WHERE workspace_id=$1 AND archived_at IS NULL AND team_id IS NULL
		ORDER BY updated_at DESC, id
	`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list unresolved projects: %w", err)
	}
	defer rows.Close()
	result := make([]Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan unresolved project: %w", err)
		}
		result = append(result, project)
	}
	return result, rows.Err()
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

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrInvalidName
	}
	return name, nil
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
