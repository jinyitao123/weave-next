package org

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTeamNotFound indicates that a workspace-scoped team does not exist.
var ErrTeamNotFound = errors.New("team not found")

// ErrTeamArchiveBlocked means the team still owns Projects or active
// collaboration bindings and cannot be archived.
var ErrTeamArchiveBlocked = errors.New("team archive blocked by projects")

// ErrArchivedTeamImmutable prevents audit history from being rewritten after
// a team leaves the active organization directory.
var ErrArchivedTeamImmutable = errors.New("archived team is immutable")

// ErrInvalidTeamDispatchRules indicates that a dispatch rule contains a
// negative numeric value.
var ErrInvalidTeamDispatchRules = errors.New("team dispatch rule values must be non-negative")

// ErrQuorumExceedsTeamSize indicates that a rule requires more workers than
// the team currently contains.
var ErrQuorumExceedsTeamSize = errors.New("team dispatch quorum exceeds worker count")

// ErrAmbiguousTeamDispatch indicates that an avatar manages workers from more
// than one team, so no single team rule can be selected.
var ErrAmbiguousTeamDispatch = errors.New("avatar dispatch team is ambiguous")

const (
	defaultLegTimeoutSec    = 180
	defaultGroupDeadlineSec = 480

	TeamEvaluationUnevaluated = "unevaluated"
	TeamEvaluationEvaluated   = "evaluated"
)

// Workspace represents an isolated workspace.
type Workspace struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Member represents a user's membership in a workspace.
type Member struct {
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Deleted     bool      `json:"deleted"`
}

// Team represents a named team within a workspace.
type Team struct {
	ID                     string     `json:"id"`
	WorkspaceID            string     `json:"workspace_id"`
	Name                   string     `json:"name"`
	Objective              string     `json:"objective"`
	PrimaryScenario        string     `json:"primary_scenario"`
	SuccessCriteria        string     `json:"success_criteria"`
	LeadAvatarID           string     `json:"lead_avatar_id"`
	Status                 string     `json:"status"`
	Evaluation             string     `json:"evaluation"`
	EvaluationBuildRunID   string     `json:"evaluation_build_run_id,omitempty"`
	EvaluationContractHash string     `json:"evaluation_contract_hash,omitempty"`
	EvaluatedAt            *time.Time `json:"evaluated_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

// UpdateTeamDesignInput contains the design-level fields that an optimize
// compiler is allowed to refresh for an existing team. It deliberately excludes
// name, lead, status, and roster membership: those remain separate governance
// surfaces.
type UpdateTeamDesignInput struct {
	Objective       string `json:"objective"`
	PrimaryScenario string `json:"primary_scenario"`
	SuccessCriteria string `json:"success_criteria"`
}

// TeamDispatchRules controls parallel dispatch for one team.
type TeamDispatchRules struct {
	TeamID           string `json:"team_id"`
	LegTimeoutSec    int    `json:"leg_timeout_sec"`
	GroupDeadlineSec int    `json:"group_deadline_sec"`
	Quorum           int    `json:"quorum"`
}

// RestoreTeamState is the baseline-only team field set restored by one
// rollback transaction: the mutable identity/scenario fields, the lifecycle
// status, and the parallel dispatch rules. Lead and workers are restored by
// the roster command in the same transaction.
type RestoreTeamState struct {
	Name            string
	Objective       string
	PrimaryScenario string
	SuccessCriteria string
	Status          string
	DispatchRules   TeamDispatchRules
}

// Store provides workspace-scoped organization operations.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore creates an organization store.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// GetWorkspace returns a workspace by ID.
func (s *Store) GetWorkspace(ctx context.Context, id string) (Workspace, error) {
	var workspace Workspace
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, created_at FROM weave_workspaces WHERE id=$1`, id,
	).Scan(&workspace.ID, &workspace.Slug, &workspace.Name, &workspace.CreatedAt)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace %q not found", id)
	}
	return workspace, nil
}

// ListMembers returns all members in a workspace.
func (s *Store) ListMembers(ctx context.Context, workspaceID string) ([]Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.workspace_id, m.user_id, m.role, m.created_at,
		        COALESCE(u.username, ''), COALESCE(u.display_name, ''), u.id IS NULL
		 FROM weave_members m
		 LEFT JOIN weave_users u ON u.id=m.user_id AND u.tenant_id=m.workspace_id
		 WHERE m.workspace_id=$1
		 ORDER BY m.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	members := make([]Member, 0)
	for rows.Next() {
		var member Member
		if err := rows.Scan(
			&member.WorkspaceID,
			&member.UserID,
			&member.Role,
			&member.CreatedAt,
			&member.Username,
			&member.DisplayName,
			&member.Deleted,
		); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, rows.Err()
}

// AddMember adds or updates a member in a workspace.
func (s *Store) AddMember(ctx context.Context, workspaceID, userID, role string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO weave_members (workspace_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET role=EXCLUDED.role
	`, workspaceID, userID, role)
	return err
}

// RemoveMember removes a member from a workspace.
func (s *Store) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM weave_members WHERE workspace_id=$1 AND user_id=$2`, workspaceID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("member %q not found", userID)
	}
	return nil
}

// ListTeams returns all teams in a workspace.
func (s *Store) ListTeams(ctx context.Context, workspaceID string) ([]Team, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, name, objective, primary_scenario,
		        success_criteria, COALESCE(lead_avatar_id, ''), status,
		        evaluation, COALESCE(evaluation_build_run_id, ''),
		        COALESCE(evaluation_contract_hash, ''), evaluated_at,
		        created_at, updated_at
		 FROM weave_teams WHERE workspace_id=$1 ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	teams := make([]Team, 0)
	for rows.Next() {
		var team Team
		if err := rows.Scan(
			&team.ID, &team.WorkspaceID, &team.Name, &team.Objective,
			&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
			&team.Status, &team.Evaluation, &team.EvaluationBuildRunID,
			&team.EvaluationContractHash, &team.EvaluatedAt,
			&team.CreatedAt, &team.UpdatedAt,
		); err != nil {
			return nil, err
		}
		teams = append(teams, team)
	}
	return teams, rows.Err()
}

// ListBusinessTeams lists teams excluding built-in platform assets (the "__"
// reserved prefix). This is the read path for surfaces that must never expose
// platform teams, such as the teamforge tf_list_teams tool.
func (s *Store) ListBusinessTeams(ctx context.Context, workspaceID string) ([]Team, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, name, objective, primary_scenario,
		        success_criteria, COALESCE(lead_avatar_id, ''), status,
		        evaluation, COALESCE(evaluation_build_run_id, ''),
		        COALESCE(evaluation_contract_hash, ''), evaluated_at,
		        created_at, updated_at
		 FROM weave_teams
		 WHERE workspace_id=$1 AND name NOT LIKE '\_\_%' ESCAPE '\' AND id NOT LIKE '\_\_%' ESCAPE '\'
		 ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	teams := make([]Team, 0)
	for rows.Next() {
		var team Team
		if err := rows.Scan(
			&team.ID, &team.WorkspaceID, &team.Name, &team.Objective,
			&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
			&team.Status, &team.Evaluation, &team.EvaluationBuildRunID,
			&team.EvaluationContractHash, &team.EvaluatedAt,
			&team.CreatedAt, &team.UpdatedAt,
		); err != nil {
			return nil, err
		}
		teams = append(teams, team)
	}
	return teams, rows.Err()
}

// CreateTeam creates a team in a workspace.
func (s *Store) CreateTeam(ctx context.Context, workspaceID, name string) (Team, error) {
	team := Team{
		ID: uuid.NewString(), WorkspaceID: workspaceID, Name: name,
		Status: "needs_repair", Evaluation: TeamEvaluationEvaluated,
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO weave_teams (id, workspace_id, name)
		VALUES ($1, $2, $3)
		RETURNING created_at, updated_at
	`, team.ID, team.WorkspaceID, team.Name).Scan(&team.CreatedAt, &team.UpdatedAt)
	if err != nil {
		return Team{}, err
	}
	return team, nil
}

// RenameTeam renames a team within a workspace.
func (s *Store) RenameTeam(ctx context.Context, workspaceID, id, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedWorkspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id FROM weave_workspaces WHERE id=$1 FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("team %q not found: %w", id, ErrTeamNotFound)
		}
		return err
	}
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, id).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("team %q not found: %w", id, ErrTeamNotFound)
		}
		return err
	}
	if status == "archived" {
		return fmt.Errorf("%w: team %q", ErrArchivedTeamImmutable, id)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_teams SET name=$3, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Deprecated: ArchiveTeam has no production caller after archive moved into
// the roster command transaction. It remains for existing tests and will be
// deleted by the follow-up cleanup ticket.
func (s *Store) ArchiveTeam(ctx context.Context, workspaceID, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin archive team: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedWorkspaceID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("team %q not found: %w", id, ErrTeamNotFound)
	}
	if err != nil {
		return fmt.Errorf("lock team workspace %q: %w", workspaceID, err)
	}

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, id).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("team %q not found: %w", id, ErrTeamNotFound)
	}
	if err != nil {
		return fmt.Errorf("lock team %q for archive: %w", id, err)
	}
	if status == "archived" {
		return tx.Commit(ctx)
	}
	if err := ensureTeamArchiveAllowed(ctx, tx, workspaceID, id); err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE weave_teams
		SET status='archived', updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, id)
	if err != nil {
		return fmt.Errorf("archive team %q: %w", id, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit archive team %q: %w", id, err)
	}
	return nil
}

func ensureTeamArchiveAllowed(ctx context.Context, tx pgx.Tx, workspaceID, teamID string) error {
	rows, err := tx.Query(ctx, `
		SELECT 'owner:' || id
		FROM weave_projects
		WHERE workspace_id=$1 AND team_id=$2 AND archived_at IS NULL
		UNION ALL
		SELECT 'collaborator:' || project_id
		FROM weave_project_collaborators
		WHERE workspace_id=$1 AND team_id=$2 AND removed_at IS NULL
		ORDER BY 1
		LIMIT 20
	`, workspaceID, teamID)
	if err != nil {
		return fmt.Errorf("check team archive blockers: %w", err)
	}
	defer rows.Close()
	blockers := make([]string, 0)
	for rows.Next() {
		var blocker string
		if err := rows.Scan(&blocker); err != nil {
			return fmt.Errorf("scan team archive blockers: %w", err)
		}
		blockers = append(blockers, blocker)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check team archive blockers: %w", err)
	}
	if len(blockers) > 0 {
		return fmt.Errorf("%w: team %q blockers=%s", ErrTeamArchiveBlocked, teamID, strings.Join(blockers, ","))
	}
	return nil
}

// GetTeamDispatchRules returns a team's configured rules or the platform
// defaults when the team has no stored rule row.
func (s *Store) GetTeamDispatchRules(ctx context.Context, workspaceID, teamID string) (TeamDispatchRules, error) {
	rules := TeamDispatchRules{TeamID: teamID}
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(r.leg_timeout_sec, $3),
		       COALESCE(r.group_deadline_sec, $4),
		       COALESCE(r.quorum, 0)
		FROM weave_teams AS t
		LEFT JOIN weave_team_dispatch_rules AS r ON r.team_id=t.id
		WHERE t.workspace_id=$1 AND t.id=$2
	`, workspaceID, teamID, defaultLegTimeoutSec, defaultGroupDeadlineSec).Scan(
		&rules.LegTimeoutSec, &rules.GroupDeadlineSec, &rules.Quorum,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamDispatchRules{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
	}
	if err != nil {
		return TeamDispatchRules{}, fmt.Errorf("get team dispatch rules: %w", err)
	}
	return rules, nil
}

// GetTeamTx returns one exact team from the requested workspace and locks the
// row FOR SHARE inside the caller-owned transaction. It is the exact read
// helper used by the team build baseline builder; callers must commit or
// roll back the transaction themselves.
func (s *Store) GetTeamTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) (Team, error) {
	if tx == nil {
		return Team{}, errors.New("get team: transaction is required")
	}
	var team Team
	err := tx.QueryRow(ctx, `
		SELECT id, workspace_id, name, objective, primary_scenario,
		       success_criteria, COALESCE(lead_avatar_id, ''), status,
		       evaluation, COALESCE(evaluation_build_run_id, ''),
		       COALESCE(evaluation_contract_hash, ''), evaluated_at,
		       created_at, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, teamID).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
	}
	if err != nil {
		return Team{}, fmt.Errorf("get team: %w", err)
	}
	if team.ID != teamID || team.WorkspaceID != workspaceID {
		return Team{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
	}
	return team, nil
}

// GetTeam returns one exact team from the requested workspace without taking
// row locks. The baseline builder uses it as a fast-fail existence check
// before acquiring the locked reads inside its authorize transaction.
func (s *Store) GetTeam(ctx context.Context, workspaceID, teamID string) (Team, error) {
	var team Team
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, name, objective, primary_scenario,
		       success_criteria, COALESCE(lead_avatar_id, ''), status,
		       evaluation, COALESCE(evaluation_build_run_id, ''),
		       COALESCE(evaluation_contract_hash, ''), evaluated_at,
		       created_at, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, teamID).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
	}
	if err != nil {
		return Team{}, fmt.Errorf("get team: %w", err)
	}
	if team.ID != teamID || team.WorkspaceID != workspaceID {
		return Team{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
	}
	return team, nil
}

// UpdateTeamDesign refreshes only the existing team's design contract fields.
// It locks workspace then team in the same order as roster writers and restore
// transactions. Replaying the same values is a content-idempotent no-op.
func (s *Store) UpdateTeamDesign(
	ctx context.Context,
	workspaceID, teamID string,
	input UpdateTeamDesignInput,
) (Team, error) {
	if workspaceID == "" || teamID == "" {
		return Team{}, errors.New("update team design: workspace_id and team_id are required")
	}
	input.Objective = strings.TrimSpace(input.Objective)
	input.PrimaryScenario = strings.TrimSpace(input.PrimaryScenario)
	input.SuccessCriteria = strings.TrimSpace(input.SuccessCriteria)
	if input.Objective == "" || input.PrimaryScenario == "" || input.SuccessCriteria == "" {
		return Team{}, errors.New("update team design: objective, primary_scenario, and success_criteria are required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Team{}, fmt.Errorf("begin update team design: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var lockedWorkspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Team{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
		}
		return Team{}, fmt.Errorf("lock team workspace: %w", err)
	}

	var team Team
	err = tx.QueryRow(ctx, `
		SELECT id, workspace_id, name, objective, primary_scenario,
		       success_criteria, COALESCE(lead_avatar_id, ''), status,
		       evaluation, COALESCE(evaluation_build_run_id, ''),
		       COALESCE(evaluation_contract_hash, ''), evaluated_at,
		       created_at, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, teamID).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
	}
	if err != nil {
		return Team{}, fmt.Errorf("lock team for design update: %w", err)
	}
	if team.Status == "archived" {
		return Team{}, fmt.Errorf("%w: team %q", ErrArchivedTeamImmutable, teamID)
	}
	if team.Objective == input.Objective &&
		team.PrimaryScenario == input.PrimaryScenario &&
		team.SuccessCriteria == input.SuccessCriteria {
		if err := tx.Commit(ctx); err != nil {
			return Team{}, fmt.Errorf("commit unchanged team design: %w", err)
		}
		return team, nil
	}

	if err := tx.QueryRow(ctx, `
		UPDATE weave_teams
		SET objective=$3, primary_scenario=$4, success_criteria=$5, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
		RETURNING id, workspace_id, name, objective, primary_scenario,
		          success_criteria, COALESCE(lead_avatar_id, ''), status,
		          evaluation, COALESCE(evaluation_build_run_id, ''),
		          COALESCE(evaluation_contract_hash, ''), evaluated_at,
		          created_at, updated_at
	`, workspaceID, teamID, input.Objective, input.PrimaryScenario, input.SuccessCriteria).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	); err != nil {
		return Team{}, fmt.Errorf("update team design: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Team{}, fmt.Errorf("commit update team design: %w", err)
	}
	return team, nil
}

// GetTeamDispatchRulesTx returns a team's configured rules or the platform
// defaults inside the caller-owned transaction, locking the team row FOR
// SHARE. It mirrors GetTeamDispatchRules but is safe to call from a
// transaction that also captures a team baseline snapshot.
func (s *Store) GetTeamDispatchRulesTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) (TeamDispatchRules, error) {
	if tx == nil {
		return TeamDispatchRules{}, errors.New("get team dispatch rules: transaction is required")
	}
	rules := TeamDispatchRules{TeamID: teamID}
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(r.leg_timeout_sec, $3),
		       COALESCE(r.group_deadline_sec, $4),
		       COALESCE(r.quorum, 0)
		FROM weave_teams AS t
		LEFT JOIN weave_team_dispatch_rules AS r ON r.team_id=t.id
		WHERE t.workspace_id=$1 AND t.id=$2
		FOR SHARE OF t
	`, workspaceID, teamID, defaultLegTimeoutSec, defaultGroupDeadlineSec).Scan(
		&rules.LegTimeoutSec, &rules.GroupDeadlineSec, &rules.Quorum,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamDispatchRules{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
	}
	if err != nil {
		return TeamDispatchRules{}, fmt.Errorf("get team dispatch rules: %w", err)
	}
	return rules, nil
}

// RestoreTeamTx restores the baseline-only team fields (name, objective,
// primary scenario, success criteria, status) and dispatch rules inside the
// caller-owned transaction. It locks the workspace row then the team row in
// the same order as roster writers so a rollback transaction cannot deadlock
// against a concurrent roster command. The restore is content-idempotent:
// replaying the same baseline fields when the team already carries them is a
// no-op and returns the current team.
func (s *Store) RestoreTeamTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
	state RestoreTeamState,
) (Team, error) {
	if tx == nil {
		return Team{}, errors.New("restore team: transaction is required")
	}
	if workspaceID == "" || teamID == "" {
		return Team{}, errors.New("restore team: workspace_id and team_id are required")
	}
	if state.Name == "" || state.Status == "" {
		return Team{}, errors.New("restore team: baseline name and status are required")
	}
	rules := state.DispatchRules
	if rules.LegTimeoutSec < 0 || rules.GroupDeadlineSec < 0 || rules.Quorum < 0 {
		return Team{}, ErrInvalidTeamDispatchRules
	}

	var lockedWorkspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Team{}, fmt.Errorf("team %q not found: %w", teamID, ErrTeamNotFound)
		}
		return Team{}, fmt.Errorf("lock team workspace: %w", err)
	}

	team, err := s.GetTeamTx(ctx, tx, workspaceID, teamID)
	if err != nil {
		return Team{}, err
	}
	currentRules, err := s.GetTeamDispatchRulesTx(ctx, tx, workspaceID, teamID)
	if err != nil {
		return Team{}, fmt.Errorf("restore team: read current dispatch rules: %w", err)
	}

	fieldsEqual := team.Name == state.Name &&
		team.Objective == state.Objective &&
		team.PrimaryScenario == state.PrimaryScenario &&
		team.SuccessCriteria == state.SuccessCriteria &&
		team.Status == state.Status
	rulesEqual := currentRules.LegTimeoutSec == rules.LegTimeoutSec &&
		currentRules.GroupDeadlineSec == rules.GroupDeadlineSec &&
		currentRules.Quorum == rules.Quorum
	if fieldsEqual && rulesEqual {
		return team, nil
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_dispatch_rules (
			team_id, leg_timeout_sec, group_deadline_sec, quorum
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (team_id) DO UPDATE SET
			leg_timeout_sec=EXCLUDED.leg_timeout_sec,
			group_deadline_sec=EXCLUDED.group_deadline_sec,
			quorum=EXCLUDED.quorum
	`, teamID, rules.LegTimeoutSec, rules.GroupDeadlineSec, rules.Quorum); err != nil {
		return Team{}, fmt.Errorf("restore team dispatch rules: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_teams
		SET name=$3, objective=$4, primary_scenario=$5,
			success_criteria=$6, status=$7, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, teamID, state.Name, state.Objective,
		state.PrimaryScenario, state.SuccessCriteria, state.Status); err != nil {
		return Team{}, fmt.Errorf("restore team fields: %w", err)
	}
	restored, err := s.GetTeamTx(ctx, tx, workspaceID, teamID)
	if err != nil {
		return Team{}, fmt.Errorf("restore team: reread restored team: %w", err)
	}
	return restored, nil
}

// PutTeamDispatchRules fully replaces one team's parallel dispatch rules.
func (s *Store) PutTeamDispatchRules(ctx context.Context, workspaceID string, rules TeamDispatchRules) error {
	if rules.LegTimeoutSec < 0 || rules.GroupDeadlineSec < 0 || rules.Quorum < 0 {
		return ErrInvalidTeamDispatchRules
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedWorkspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id FROM weave_workspaces WHERE id=$1 FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("team %q not found: %w", rules.TeamID, ErrTeamNotFound)
		}
		return err
	}
	var status string
	if err := tx.QueryRow(ctx, `
		SELECT status FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, rules.TeamID).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("team %q not found: %w", rules.TeamID, ErrTeamNotFound)
		}
		return err
	}
	if status == "archived" {
		return fmt.Errorf("%w: team %q", ErrArchivedTeamImmutable, rules.TeamID)
	}

	var workerCount int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2 AND enabled=true
	`, workspaceID, rules.TeamID).Scan(&workerCount); err != nil {
		return fmt.Errorf("count team workers: %w", err)
	}
	if rules.Quorum > workerCount {
		return fmt.Errorf("%w: quorum %d exceeds worker count %d", ErrQuorumExceedsTeamSize, rules.Quorum, workerCount)
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO weave_team_dispatch_rules (
			team_id, leg_timeout_sec, group_deadline_sec, quorum
		) VALUES ($1, $2, $3, $4)
		ON CONFLICT (team_id) DO UPDATE SET
			leg_timeout_sec=EXCLUDED.leg_timeout_sec,
			group_deadline_sec=EXCLUDED.group_deadline_sec,
			quorum=EXCLUDED.quorum
	`, rules.TeamID, rules.LegTimeoutSec, rules.GroupDeadlineSec, rules.Quorum)
	if err != nil {
		return fmt.Errorf("put team dispatch rules: %w", err)
	}
	return tx.Commit(ctx)
}

// ResolveTeamDispatchRules finds the single team represented by an avatar's
// managed workers. The boolean reports whether that team has a stored rule row;
// callers can retain their existing defaults when it is false.
func (s *Store) ResolveTeamDispatchRules(
	ctx context.Context,
	workspaceID, avatarAgent string,
) (TeamDispatchRules, bool, error) {
	if s == nil {
		return TeamDispatchRules{}, false, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT worker.team_id
		FROM weave_agents AS avatar
		JOIN weave_agent_links AS link
		  ON link.workspace_id=avatar.workspace_id
		 AND link.from_agent_id=avatar.id
		 AND link.type='manages'
		JOIN weave_agents AS worker
		  ON worker.workspace_id=link.workspace_id
		 AND worker.id=link.to_agent_id
		WHERE avatar.workspace_id=$1 AND avatar.name=$2 AND avatar.deleted=false
		  AND worker.deleted=false AND worker.team_id IS NOT NULL
		ORDER BY worker.team_id
	`, workspaceID, avatarAgent)
	if err != nil {
		return TeamDispatchRules{}, false, fmt.Errorf("resolve avatar dispatch team: %w", err)
	}
	defer rows.Close()

	teamIDs := make([]string, 0, 2)
	for rows.Next() {
		var teamID string
		if err := rows.Scan(&teamID); err != nil {
			return TeamDispatchRules{}, false, fmt.Errorf("scan avatar dispatch team: %w", err)
		}
		teamIDs = append(teamIDs, teamID)
	}
	if err := rows.Err(); err != nil {
		return TeamDispatchRules{}, false, fmt.Errorf("resolve avatar dispatch team: %w", err)
	}
	if len(teamIDs) > 1 {
		return TeamDispatchRules{}, false, fmt.Errorf(
			"%w: avatar %q manages workers from teams %q and %q",
			ErrAmbiguousTeamDispatch, avatarAgent, teamIDs[0], teamIDs[1],
		)
	}
	if len(teamIDs) == 0 {
		return TeamDispatchRules{}, false, nil
	}

	rules, err := s.GetTeamDispatchRules(ctx, workspaceID, teamIDs[0])
	if err != nil {
		return TeamDispatchRules{}, false, err
	}
	var configured bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM weave_team_dispatch_rules WHERE team_id=$1
		)
	`, teamIDs[0]).Scan(&configured); err != nil {
		return TeamDispatchRules{}, false, fmt.Errorf("check team dispatch rules: %w", err)
	}
	return rules, configured, nil
}
