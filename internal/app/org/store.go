package org

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/orgspec"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultLegTimeoutSec    = 180
	defaultGroupDeadlineSec = 480
)

// Store provides workspace-scoped organization operations.
type Store struct {
	pool           *pgxpool.Pool
	memberProfiles MemberProfiles
}

// NewStore creates an organization store.
func NewStore(pool *pgxpool.Pool, options ...StoreOption) *Store {
	s := &Store{pool: pool}
	for _, option := range options {
		option(s)
	}
	return s
}

// GetWorkspace returns a workspace by ID.
func (s *Store) GetWorkspace(ctx context.Context, id string) (orgspec.Workspace, error) {
	var workspace orgspec.Workspace
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, created_at FROM weave_workspaces WHERE id=$1`, id,
	).Scan(&workspace.ID, &workspace.Slug, &workspace.Name, &workspace.CreatedAt)
	if err != nil {
		return orgspec.Workspace{}, fmt.Errorf("workspace %q not found", id)
	}
	return workspace, nil
}

// ListMembers returns all members in a workspace.
func (s *Store) ListMembers(ctx context.Context, workspaceID string) ([]orgspec.Member, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT m.workspace_id, m.user_id, m.role, m.created_at
		 FROM weave_members m
		 WHERE m.workspace_id=$1
		 ORDER BY m.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	members := make([]orgspec.Member, 0)
	for rows.Next() {
		var member orgspec.Member
		if err := rows.Scan(
			&member.WorkspaceID,
			&member.UserID,
			&member.Role,
			&member.CreatedAt,
		); err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if len(members) == 0 {
		return members, nil
	}
	if s.memberProfiles == nil {
		return nil, ErrMemberProfilesUnavailable
	}
	ids := make([]string, len(members))
	for i := range members {
		ids[i] = members[i].UserID
	}
	profiles, err := s.memberProfiles(ctx, workspaceID, ids)
	if err != nil {
		return nil, fmt.Errorf("read member profiles: %w", err)
	}
	for i := range members {
		profile, found := profiles[members[i].UserID]
		members[i].Username, members[i].DisplayName, members[i].Deleted = profile.Username, profile.DisplayName, !found
	}
	return members, nil
}

// AddMember adds or updates a member in a workspace.
func (s *Store) AddMember(ctx context.Context, workspaceID, userID, role string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.AddMemberTx(ctx, tx, workspaceID, userID, role); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) RemoveMember(ctx context.Context, workspaceID, userID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = s.RemoveMemberTx(ctx, tx, workspaceID, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Store) AddMemberTx(ctx context.Context, tx pgx.Tx, workspaceID, userID, role string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO weave_members (workspace_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, user_id) DO UPDATE SET role=EXCLUDED.role
	`, workspaceID, userID, role)
	return err
}

// RemoveMember removes a member from a workspace.
func (s *Store) RemoveMemberTx(ctx context.Context, tx pgx.Tx, workspaceID, userID string) error {
	tag, err := tx.Exec(ctx,
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
func (s *Store) ListTeams(ctx context.Context, workspaceID string) ([]orgspec.Team, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, name, COALESCE(NULLIF(display_name, ''), name), objective, primary_scenario,
		        success_criteria, COALESCE(lead_avatar_id, ''), status,
		        COALESCE(default_workflow_id, ''),
		        evaluation, COALESCE(evaluation_build_run_id, ''),
		        COALESCE(evaluation_contract_hash, ''), evaluated_at,
		        created_at, updated_at
		 FROM weave_teams WHERE workspace_id=$1 ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	teams := make([]orgspec.Team, 0)
	for rows.Next() {
		var team orgspec.Team
		if err := rows.Scan(
			&team.ID, &team.WorkspaceID, &team.Name, &team.DisplayName, &team.Objective,
			&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
			&team.Status, &team.DefaultWorkflowID, &team.Evaluation, &team.EvaluationBuildRunID,
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
func (s *Store) ListBusinessTeams(ctx context.Context, workspaceID string) ([]orgspec.Team, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, name, COALESCE(NULLIF(display_name, ''), name), objective, primary_scenario,
		        success_criteria, COALESCE(lead_avatar_id, ''), status,
		        COALESCE(default_workflow_id, ''),
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

	teams := make([]orgspec.Team, 0)
	for rows.Next() {
		var team orgspec.Team
		if err := rows.Scan(
			&team.ID, &team.WorkspaceID, &team.Name, &team.DisplayName, &team.Objective,
			&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
			&team.Status, &team.DefaultWorkflowID, &team.Evaluation, &team.EvaluationBuildRunID,
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
func (s *Store) CreateTeam(ctx context.Context, workspaceID, name string) (orgspec.Team, error) {
	team := orgspec.Team{
		ID: uuid.NewString(), WorkspaceID: workspaceID, Name: name, DisplayName: name,
		Status: "needs_repair", Evaluation: orgspec.TeamEvaluationEvaluated,
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO weave_teams (id, workspace_id, name, display_name)
		VALUES ($1, $2, $3, $4)
		RETURNING created_at, updated_at
	`, team.ID, team.WorkspaceID, team.Name, team.DisplayName).Scan(&team.CreatedAt, &team.UpdatedAt)
	if err != nil {
		return orgspec.Team{}, err
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
			return fmt.Errorf("team %q not found: %w", id, orgspec.ErrTeamNotFound)
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
			return fmt.Errorf("team %q not found: %w", id, orgspec.ErrTeamNotFound)
		}
		return err
	}
	if status == "archived" {
		return fmt.Errorf("%w: team %q", orgspec.ErrArchivedTeamImmutable, id)
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
		return fmt.Errorf("team %q not found: %w", id, orgspec.ErrTeamNotFound)
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
		return fmt.Errorf("team %q not found: %w", id, orgspec.ErrTeamNotFound)
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
		return fmt.Errorf("%w: team %q blockers=%s", orgspec.ErrTeamArchiveBlocked, teamID, strings.Join(blockers, ","))
	}
	return nil
}

// GetTeamDispatchRules returns a team's configured rules or the platform
// defaults when the team has no stored rule row.
func (s *Store) GetTeamDispatchRules(ctx context.Context, workspaceID, teamID string) (orgspec.TeamDispatchRules, error) {
	rules := orgspec.TeamDispatchRules{TeamID: teamID}
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
		return orgspec.TeamDispatchRules{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
	}
	if err != nil {
		return orgspec.TeamDispatchRules{}, fmt.Errorf("get team dispatch rules: %w", err)
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
) (orgspec.Team, error) {
	if tx == nil {
		return orgspec.Team{}, errors.New("get team: transaction is required")
	}
	var team orgspec.Team
	err := tx.QueryRow(ctx, `
		SELECT id, workspace_id, name, COALESCE(NULLIF(display_name, ''), name), objective, primary_scenario,
		       success_criteria, COALESCE(lead_avatar_id, ''), status,
		       COALESCE(default_workflow_id, ''),
		       evaluation, COALESCE(evaluation_build_run_id, ''),
		       COALESCE(evaluation_contract_hash, ''), evaluated_at,
		       created_at, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, teamID).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.DisplayName, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.DefaultWorkflowID, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return orgspec.Team{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
	}
	if err != nil {
		return orgspec.Team{}, fmt.Errorf("get team: %w", err)
	}
	if team.ID != teamID || team.WorkspaceID != workspaceID {
		return orgspec.Team{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
	}
	return team, nil
}

// GetTeam returns one exact team from the requested workspace without taking
// row locks. The baseline builder uses it as a fast-fail existence check
// before acquiring the locked reads inside its authorize transaction.
func (s *Store) GetTeam(ctx context.Context, workspaceID, teamID string) (orgspec.Team, error) {
	var team orgspec.Team
	err := s.pool.QueryRow(ctx, `
		SELECT id, workspace_id, name, COALESCE(NULLIF(display_name, ''), name), objective, primary_scenario,
		       success_criteria, COALESCE(lead_avatar_id, ''), status,
		       COALESCE(default_workflow_id, ''),
		       evaluation, COALESCE(evaluation_build_run_id, ''),
		       COALESCE(evaluation_contract_hash, ''), evaluated_at,
		       created_at, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, teamID).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.DisplayName, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.DefaultWorkflowID, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return orgspec.Team{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
	}
	if err != nil {
		return orgspec.Team{}, fmt.Errorf("get team: %w", err)
	}
	if team.ID != teamID || team.WorkspaceID != workspaceID {
		return orgspec.Team{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
	}
	return team, nil
}

// UpdateTeamDesign refreshes only the existing team's design contract fields.
// It locks workspace then team in the same order as roster writers and restore
// transactions. Replaying the same values is a content-idempotent no-op.
func (s *Store) UpdateTeamDesign(
	ctx context.Context,
	workspaceID, teamID string,
	input orgspec.UpdateTeamDesignInput,
) (orgspec.Team, error) {
	if workspaceID == "" || teamID == "" {
		return orgspec.Team{}, errors.New("update team design: workspace_id and team_id are required")
	}
	input.Objective = strings.TrimSpace(input.Objective)
	input.PrimaryScenario = strings.TrimSpace(input.PrimaryScenario)
	input.SuccessCriteria = strings.TrimSpace(input.SuccessCriteria)
	if input.Objective == "" || input.PrimaryScenario == "" || input.SuccessCriteria == "" {
		return orgspec.Team{}, errors.New("update team design: objective, primary_scenario, and success_criteria are required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return orgspec.Team{}, fmt.Errorf("begin update team design: %w", err)
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
			return orgspec.Team{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
		}
		return orgspec.Team{}, fmt.Errorf("lock team workspace: %w", err)
	}

	var team orgspec.Team
	err = tx.QueryRow(ctx, `
		SELECT id, workspace_id, name, COALESCE(NULLIF(display_name, ''), name), objective, primary_scenario,
		       success_criteria, COALESCE(lead_avatar_id, ''), status,
		       COALESCE(default_workflow_id, ''),
		       evaluation, COALESCE(evaluation_build_run_id, ''),
		       COALESCE(evaluation_contract_hash, ''), evaluated_at,
		       created_at, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, teamID).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.DisplayName, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.DefaultWorkflowID, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return orgspec.Team{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
	}
	if err != nil {
		return orgspec.Team{}, fmt.Errorf("lock team for design update: %w", err)
	}
	if team.Status == "archived" {
		return orgspec.Team{}, fmt.Errorf("%w: team %q", orgspec.ErrArchivedTeamImmutable, teamID)
	}
	if team.Objective == input.Objective &&
		team.PrimaryScenario == input.PrimaryScenario &&
		team.SuccessCriteria == input.SuccessCriteria {
		if err := tx.Commit(ctx); err != nil {
			return orgspec.Team{}, fmt.Errorf("commit unchanged team design: %w", err)
		}
		return team, nil
	}

	if err := tx.QueryRow(ctx, `
		UPDATE weave_teams
		SET objective=$3, primary_scenario=$4, success_criteria=$5, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
		RETURNING id, workspace_id, name, COALESCE(NULLIF(display_name, ''), name), objective, primary_scenario,
		          success_criteria, COALESCE(lead_avatar_id, ''), status,
		          COALESCE(default_workflow_id, ''),
		          evaluation, COALESCE(evaluation_build_run_id, ''),
		          COALESCE(evaluation_contract_hash, ''), evaluated_at,
		          created_at, updated_at
	`, workspaceID, teamID, input.Objective, input.PrimaryScenario, input.SuccessCriteria).Scan(
		&team.ID, &team.WorkspaceID, &team.Name, &team.DisplayName, &team.Objective,
		&team.PrimaryScenario, &team.SuccessCriteria, &team.LeadAvatarID,
		&team.Status, &team.DefaultWorkflowID, &team.Evaluation, &team.EvaluationBuildRunID,
		&team.EvaluationContractHash, &team.EvaluatedAt,
		&team.CreatedAt, &team.UpdatedAt,
	); err != nil {
		return orgspec.Team{}, fmt.Errorf("update team design: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return orgspec.Team{}, fmt.Errorf("commit update team design: %w", err)
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
) (orgspec.TeamDispatchRules, error) {
	if tx == nil {
		return orgspec.TeamDispatchRules{}, errors.New("get team dispatch rules: transaction is required")
	}
	rules := orgspec.TeamDispatchRules{TeamID: teamID}
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
		return orgspec.TeamDispatchRules{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
	}
	if err != nil {
		return orgspec.TeamDispatchRules{}, fmt.Errorf("get team dispatch rules: %w", err)
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
	state orgspec.RestoreTeamState,
) (orgspec.Team, error) {
	if tx == nil {
		return orgspec.Team{}, errors.New("restore team: transaction is required")
	}
	if workspaceID == "" || teamID == "" {
		return orgspec.Team{}, errors.New("restore team: workspace_id and team_id are required")
	}
	if state.Name == "" || state.Status == "" {
		return orgspec.Team{}, errors.New("restore team: baseline name and status are required")
	}
	rules := state.DispatchRules
	if rules.LegTimeoutSec < 0 || rules.GroupDeadlineSec < 0 || rules.Quorum < 0 {
		return orgspec.Team{}, orgspec.ErrInvalidTeamDispatchRules
	}

	var lockedWorkspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return orgspec.Team{}, fmt.Errorf("team %q not found: %w", teamID, orgspec.ErrTeamNotFound)
		}
		return orgspec.Team{}, fmt.Errorf("lock team workspace: %w", err)
	}

	team, err := s.GetTeamTx(ctx, tx, workspaceID, teamID)
	if err != nil {
		return orgspec.Team{}, err
	}
	currentRules, err := s.GetTeamDispatchRulesTx(ctx, tx, workspaceID, teamID)
	if err != nil {
		return orgspec.Team{}, fmt.Errorf("restore team: read current dispatch rules: %w", err)
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
		return orgspec.Team{}, fmt.Errorf("restore team dispatch rules: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE weave_teams
		SET name=$3, objective=$4, primary_scenario=$5,
			success_criteria=$6, status=$7, updated_at=now()
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, teamID, state.Name, state.Objective,
		state.PrimaryScenario, state.SuccessCriteria, state.Status); err != nil {
		return orgspec.Team{}, fmt.Errorf("restore team fields: %w", err)
	}
	restored, err := s.GetTeamTx(ctx, tx, workspaceID, teamID)
	if err != nil {
		return orgspec.Team{}, fmt.Errorf("restore team: reread restored team: %w", err)
	}
	return restored, nil
}

// PutTeamDispatchRules fully replaces one team's parallel dispatch rules.
func (s *Store) PutTeamDispatchRules(ctx context.Context, workspaceID string, rules orgspec.TeamDispatchRules) error {
	if rules.LegTimeoutSec < 0 || rules.GroupDeadlineSec < 0 || rules.Quorum < 0 {
		return orgspec.ErrInvalidTeamDispatchRules
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
			return fmt.Errorf("team %q not found: %w", rules.TeamID, orgspec.ErrTeamNotFound)
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
			return fmt.Errorf("team %q not found: %w", rules.TeamID, orgspec.ErrTeamNotFound)
		}
		return err
	}
	if status == "archived" {
		return fmt.Errorf("%w: team %q", orgspec.ErrArchivedTeamImmutable, rules.TeamID)
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
		return fmt.Errorf("%w: quorum %d exceeds worker count %d", orgspec.ErrQuorumExceedsTeamSize, rules.Quorum, workerCount)
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
) (orgspec.TeamDispatchRules, bool, error) {
	if s == nil {
		return orgspec.TeamDispatchRules{}, false, nil
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
		return orgspec.TeamDispatchRules{}, false, fmt.Errorf("resolve avatar dispatch team: %w", err)
	}
	defer rows.Close()

	teamIDs := make([]string, 0, 2)
	for rows.Next() {
		var teamID string
		if err := rows.Scan(&teamID); err != nil {
			return orgspec.TeamDispatchRules{}, false, fmt.Errorf("scan avatar dispatch team: %w", err)
		}
		teamIDs = append(teamIDs, teamID)
	}
	if err := rows.Err(); err != nil {
		return orgspec.TeamDispatchRules{}, false, fmt.Errorf("resolve avatar dispatch team: %w", err)
	}
	if len(teamIDs) > 1 {
		return orgspec.TeamDispatchRules{}, false, fmt.Errorf(
			"%w: avatar %q manages workers from teams %q and %q",
			orgspec.ErrAmbiguousTeamDispatch, avatarAgent, teamIDs[0], teamIDs[1],
		)
	}
	if len(teamIDs) == 0 {
		return orgspec.TeamDispatchRules{}, false, nil
	}

	rules, err := s.GetTeamDispatchRules(ctx, workspaceID, teamIDs[0])
	if err != nil {
		return orgspec.TeamDispatchRules{}, false, err
	}
	var configured bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM weave_team_dispatch_rules WHERE team_id=$1
		)
	`, teamIDs[0]).Scan(&configured); err != nil {
		return orgspec.TeamDispatchRules{}, false, fmt.Errorf("check team dispatch rules: %w", err)
	}
	return rules, configured, nil
}
