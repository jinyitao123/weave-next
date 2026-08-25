package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrTeamWorkerNotFound indicates that a workspace-scoped team-worker relation does not exist.
var ErrTeamWorkerNotFound = errors.New("team worker not found")

// ErrActiveTeamRequiresEnabledWorker protects the active-team roster invariant.
var ErrActiveTeamRequiresEnabledWorker = errors.New("active team requires at least one enabled worker")

// TeamWorker is one team's relationship to a reusable worker agent.
type TeamWorker struct {
	WorkspaceID        string    `json:"workspace_id"`
	TeamID             string    `json:"team_id"`
	WorkerAgentID      string    `json:"worker_agent_id"`
	Duty               string    `json:"duty"`
	WhenToUse          string    `json:"when_to_use"`
	ContextInstruction string    `json:"context_instruction"`
	AllowedKinds       []string  `json:"allowed_kinds"`
	DefaultKind        string    `json:"default_kind"`
	ResultRequirement  string    `json:"result_requirement"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// PublicationTeamRead is the authoritative team, lead version, and complete
// roster snapshot locked in a caller-owned publication transaction.
type PublicationTeamRead struct {
	WorkspaceID       string       `json:"workspace_id"`
	TeamID            string       `json:"team_id"`
	Status            string       `json:"status"`
	LeadAvatarID      string       `json:"lead_avatar_id"`
	LeadAvatarVersion int64        `json:"lead_avatar_version"`
	Workers           []TeamWorker `json:"workers"`
}

// TeamWorkerDispatchRules is the configured fanout policy for the active team
// represented by a lead avatar.
type TeamWorkerDispatchRules struct {
	TeamID           string
	LegTimeoutSec    int
	GroupDeadlineSec int
	Quorum           int
}

// TeamWorkerRepository persists workspace-scoped team-worker relations.
type TeamWorkerRepository struct {
	pool *pgxpool.Pool
}

// NewTeamWorkerRepository creates a production PostgreSQL repository.
func NewTeamWorkerRepository(pool *pgxpool.Pool) *TeamWorkerRepository {
	return &TeamWorkerRepository{pool: pool}
}

func validateTeamWorkerKinds(allowedKinds []string, defaultKind string) error {
	if len(allowedKinds) == 0 {
		return errors.New("allowed_kinds must not be empty")
	}
	allowed := make(map[string]struct{}, len(allowedKinds))
	for _, kind := range allowedKinds {
		if kind != "consult" && kind != "dispatch" && kind != "handoff" {
			return fmt.Errorf("invalid allowed kind %q", kind)
		}
		if _, exists := allowed[kind]; exists {
			return fmt.Errorf("duplicate allowed kind %q", kind)
		}
		allowed[kind] = struct{}{}
	}
	if _, exists := allowed[defaultKind]; !exists {
		return errors.New("default_kind must belong to allowed_kinds")
	}
	return nil
}

// Create inserts a team-worker relation after validating its authorization set.
func (r *TeamWorkerRepository) Create(
	ctx context.Context,
	workspaceID string,
	worker TeamWorker,
) (*TeamWorker, error) {
	if err := validateTeamWorkerKinds(worker.AllowedKinds, worker.DefaultKind); err != nil {
		return nil, err
	}
	var created *TeamWorker
	err := withLockedTeamMutation(ctx, r.pool, workspaceID, worker.TeamID, func(tx pgx.Tx, team lockedTeam) error {
		if team.Status == "archived" {
			return fmt.Errorf("%w: team %q", ErrArchivedTeamImmutable, worker.TeamID)
		}
		roles, err := lockActiveAgents(ctx, tx, workspaceID, worker.WorkerAgentID)
		if err != nil {
			return err
		}
		if roles[worker.WorkerAgentID] != "worker" {
			return fmt.Errorf("%w in workspace %q", ErrTeamWorkerNotFound, workspaceID)
		}
		if err := validateLockedTeamWorkerAgent(ctx, tx, workspaceID, worker.WorkerAgentID); err != nil {
			return err
		}
		created, err = scanTeamWorker(tx.QueryRow(ctx, `
			INSERT INTO weave_team_workers (
				workspace_id, team_id, worker_agent_id, duty, when_to_use,
				context_instruction, allowed_kinds, default_kind, result_requirement, enabled
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING workspace_id, team_id, worker_agent_id, duty, when_to_use,
			          context_instruction, allowed_kinds, default_kind, result_requirement,
			          enabled, created_at, updated_at
		`, workspaceID, worker.TeamID, worker.WorkerAgentID, worker.Duty, worker.WhenToUse,
			worker.ContextInstruction, worker.AllowedKinds, worker.DefaultKind,
			worker.ResultRequirement, worker.Enabled))
		if err != nil {
			return err
		}
		return ensureActiveTeamHasEnabledWorker(ctx, tx, workspaceID, worker.TeamID, team.Status)
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Get returns one team-worker relation, including disabled relations.
func (r *TeamWorkerRepository) Get(
	ctx context.Context,
	workspaceID, teamID, workerAgentID string,
) (*TeamWorker, error) {
	worker, err := scanTeamWorker(r.pool.QueryRow(ctx, `
		SELECT workspace_id, team_id, worker_agent_id, duty, when_to_use,
		       context_instruction, allowed_kinds, default_kind, result_requirement,
		       enabled, created_at, updated_at
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=$3
	`, workspaceID, teamID, workerAgentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: team %q worker %q", ErrTeamWorkerNotFound, teamID, workerAgentID)
	}
	return worker, err
}

// Update replaces the mutable fields of one team-worker relation.
func (r *TeamWorkerRepository) Update(
	ctx context.Context,
	workspaceID string,
	worker TeamWorker,
) (*TeamWorker, error) {
	if err := validateTeamWorkerKinds(worker.AllowedKinds, worker.DefaultKind); err != nil {
		return nil, err
	}
	var updated *TeamWorker
	err := withLockedTeamMutation(ctx, r.pool, workspaceID, worker.TeamID, func(tx pgx.Tx, team lockedTeam) error {
		if team.Status == "archived" {
			return fmt.Errorf("%w: team %q", ErrArchivedTeamImmutable, worker.TeamID)
		}
		roles, err := lockActiveAgents(ctx, tx, workspaceID, worker.WorkerAgentID)
		if err != nil {
			return err
		}
		if roles[worker.WorkerAgentID] != "worker" {
			return fmt.Errorf("%w in workspace %q", ErrTeamWorkerNotFound, workspaceID)
		}
		if err := validateLockedTeamWorkerAgent(ctx, tx, workspaceID, worker.WorkerAgentID); err != nil {
			return err
		}
		updated, err = scanTeamWorker(tx.QueryRow(ctx, `
			UPDATE weave_team_workers
			SET duty=$4, when_to_use=$5, context_instruction=$6, allowed_kinds=$7,
			    default_kind=$8, result_requirement=$9, enabled=$10, updated_at=now()
			WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=$3
			RETURNING workspace_id, team_id, worker_agent_id, duty, when_to_use,
			          context_instruction, allowed_kinds, default_kind, result_requirement,
			          enabled, created_at, updated_at
		`, workspaceID, worker.TeamID, worker.WorkerAgentID, worker.Duty, worker.WhenToUse,
			worker.ContextInstruction, worker.AllowedKinds, worker.DefaultKind,
			worker.ResultRequirement, worker.Enabled))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: team %q worker %q", ErrTeamWorkerNotFound, worker.TeamID, worker.WorkerAgentID)
		}
		if err != nil {
			return err
		}
		return ensureActiveTeamHasEnabledWorker(ctx, tx, workspaceID, worker.TeamID, team.Status)
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func validateLockedTeamWorkerAgent(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, workerAgentID string,
) error {
	var data []byte
	if err := tx.QueryRow(ctx, `
		SELECT spec
		FROM weave_agents
		WHERE workspace_id=$1 AND id=$2 AND deleted=false
	`, workspaceID, workerAgentID).Scan(&data); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w in workspace %q", ErrTeamWorkerNotFound, workspaceID)
		}
		return err
	}
	var rec AgentRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("decode team worker %q: %w", workerAgentID, err)
	}
	return ValidateTeamWorkerAgentRecord(&rec)
}

// Delete removes a team-worker relation without deleting its team or agent.
func (r *TeamWorkerRepository) Delete(
	ctx context.Context,
	workspaceID, teamID, workerAgentID string,
) error {
	return withLockedTeamMutation(ctx, r.pool, workspaceID, teamID, func(tx pgx.Tx, team lockedTeam) error {
		if team.Status == "archived" {
			return fmt.Errorf("%w: team %q", ErrArchivedTeamImmutable, teamID)
		}
		result, err := tx.Exec(ctx, `
			DELETE FROM weave_team_workers
			WHERE workspace_id=$1 AND team_id=$2 AND worker_agent_id=$3
		`, workspaceID, teamID, workerAgentID)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			return fmt.Errorf("%w: team %q worker %q", ErrTeamWorkerNotFound, teamID, workerAgentID)
		}
		return ensureActiveTeamHasEnabledWorker(ctx, tx, workspaceID, teamID, team.Status)
	})
}

type lockedTeam struct {
	Status       string
	LeadAvatarID *string
}

func withLockedTeamMutation(
	ctx context.Context,
	pool *pgxpool.Pool,
	workspaceID, teamID string,
	mutate func(pgx.Tx, lockedTeam) error,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	team, err := lockTeamForMutation(ctx, tx, workspaceID, teamID)
	if err != nil {
		return err
	}
	if err := mutate(tx, team); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockTeamForMutation(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) (lockedTeam, error) {
	if err := lockOrganizationWorkspace(ctx, tx, workspaceID); err != nil {
		return lockedTeam{}, err
	}
	var team lockedTeam
	err := tx.QueryRow(ctx, `
		SELECT status, lead_avatar_id
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, teamID).Scan(&team.Status, &team.LeadAvatarID)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedTeam{}, fmt.Errorf("%w: team %q in workspace %q", ErrTeamWorkerNotFound, teamID, workspaceID)
	}
	return team, err
}

func lockOrganizationWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var lockedID string
	err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("workspace %q not found", workspaceID)
	}
	return err
}

func ensureActiveTeamHasEnabledWorker(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID, status string,
) error {
	if status != "active" {
		return nil
	}
	var exists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM weave_team_workers
			WHERE workspace_id=$1 AND team_id=$2 AND enabled=true
		)
	`, workspaceID, teamID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: team %q", ErrActiveTeamRequiresEnabledWorker, teamID)
	}
	return nil
}

// ListByTeam returns every relation for one team, including disabled relations.
func (r *TeamWorkerRepository) ListByTeam(
	ctx context.Context,
	workspaceID, teamID string,
) ([]TeamWorker, error) {
	return r.list(ctx, `
		SELECT workspace_id, team_id, worker_agent_id, duty, when_to_use,
		       context_instruction, allowed_kinds, default_kind, result_requirement,
		       enabled, created_at, updated_at
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2
		ORDER BY worker_agent_id
	`, workspaceID, teamID)
}

// ResolveTeamWorkersForShareTx locks the exact workspace and team parent in
// stable order, then returns the complete immutable roster view using the
// caller-owned transaction. Guarded writers take the same parents FOR UPDATE.
func (r *AgentRegistry) ResolveTeamWorkersForShareTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) ([]TeamWorker, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(teamID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		teamID != strings.TrimSpace(teamID) {
		return nil, resolverError(CodeDependencyUnenumerable, nil)
	}

	var lockedWorkspaceID string
	err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR SHARE
	`, workspaceID).Scan(&lockedWorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resolverError(CodeDependencyUnenumerable, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("lock roster workspace: %w", err)
	}
	if lockedWorkspaceID != workspaceID {
		return nil, resolverError(CodeFrozenManifestMismatch, nil)
	}

	var lockedTeamID string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, teamID).Scan(&lockedTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resolverError(CodeDependencyUnenumerable, ErrTeamWorkerNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("lock roster team: %w", err)
	}
	if lockedTeamID != teamID {
		return nil, resolverError(CodeFrozenManifestMismatch, nil)
	}

	rows, err := tx.Query(ctx, `
		SELECT workspace_id, team_id, worker_agent_id, duty, when_to_use,
		       context_instruction, allowed_kinds, default_kind, result_requirement,
		       enabled, created_at, updated_at
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2
		ORDER BY worker_agent_id
		FOR SHARE
	`, workspaceID, teamID)
	if err != nil {
		return nil, fmt.Errorf("lock team roster: %w", err)
	}
	defer rows.Close()

	workers := make([]TeamWorker, 0)
	for rows.Next() {
		worker, err := scanTeamWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("scan locked team roster: %w", err)
		}
		if worker.WorkspaceID != workspaceID || worker.TeamID != teamID {
			return nil, resolverError(CodeFrozenManifestMismatch, nil)
		}
		workers = append(workers, *worker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locked team roster: %w", err)
	}
	return workers, nil
}

// ResolvePublicationTeamTx locks the authoritative publishable team, current lead
// version, and complete roster after the caller has locked the draft parents.
func (r *AgentRegistry) ResolvePublicationTeamTx(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) (*PublicationTeamRead, error) {
	if tx == nil ||
		strings.TrimSpace(workspaceID) == "" ||
		strings.TrimSpace(teamID) == "" ||
		workspaceID != strings.TrimSpace(workspaceID) ||
		teamID != strings.TrimSpace(teamID) {
		return nil, resolverError(CodeDependencyUnenumerable, nil)
	}

	var read PublicationTeamRead
	var leadAvatarID *string
	err := tx.QueryRow(ctx, `
		SELECT workspace_id, id, status, lead_avatar_id
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, teamID).Scan(
		&read.WorkspaceID,
		&read.TeamID,
		&read.Status,
		&leadAvatarID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resolverError(CodeDependencyUnenumerable, ErrTeamWorkerNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("lock publication team: %w", err)
	}
	if read.WorkspaceID != workspaceID ||
		read.TeamID != teamID ||
		(read.Status != "active" && read.Status != "building") ||
		leadAvatarID == nil ||
		strings.TrimSpace(*leadAvatarID) == "" {
		return nil, resolverError(CodeDependencyUnenumerable, nil)
	}
	read.LeadAvatarID = *leadAvatarID

	var leadWorkspaceID, lockedLeadID, leadRole string
	var leadDeleted bool
	err = tx.QueryRow(ctx, `
		SELECT workspace_id, id, role, deleted, version
		FROM weave_agents
		WHERE workspace_id=$1 AND id=$2
		FOR SHARE
	`, workspaceID, read.LeadAvatarID).Scan(
		&leadWorkspaceID,
		&lockedLeadID,
		&leadRole,
		&leadDeleted,
		&read.LeadAvatarVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resolverError(CodeDependencyUnenumerable, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("lock publication lead: %w", err)
	}
	if leadWorkspaceID != workspaceID ||
		lockedLeadID != read.LeadAvatarID ||
		leadRole != "avatar" ||
		leadDeleted ||
		read.LeadAvatarVersion < 1 {
		return nil, resolverError(CodeDependencyUnenumerable, nil)
	}

	var leadVersionData []byte
	err = tx.QueryRow(ctx, `
		SELECT spec
		FROM weave_agent_versions
		WHERE workspace_id=$1 AND agent_id=$2 AND version=$3
		FOR SHARE
	`, workspaceID, read.LeadAvatarID, read.LeadAvatarVersion).Scan(&leadVersionData)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, resolverError(CodeFrozenManifestMismatch, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("lock publication lead version: %w", err)
	}
	var leadVersion AgentRecord
	if err := json.Unmarshal(leadVersionData, &leadVersion); err != nil {
		return nil, resolverError(CodeFrozenManifestMismatch, err)
	}
	if leadVersion.WorkspaceID != workspaceID ||
		leadVersion.ID != read.LeadAvatarID ||
		int64(leadVersion.Version) != read.LeadAvatarVersion ||
		leadVersion.Role != "avatar" ||
		leadVersion.Deleted ||
		strings.TrimSpace(leadVersion.Name) == "" {
		return nil, resolverError(CodeFrozenManifestMismatch, nil)
	}

	rows, err := tx.Query(ctx, `
		SELECT workspace_id, team_id, worker_agent_id, duty, when_to_use,
		       context_instruction, allowed_kinds, default_kind, result_requirement,
		       enabled, created_at, updated_at
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2
		ORDER BY worker_agent_id
		FOR SHARE
	`, workspaceID, teamID)
	if err != nil {
		return nil, fmt.Errorf("lock publication team roster: %w", err)
	}
	defer rows.Close()

	read.Workers = make([]TeamWorker, 0)
	for rows.Next() {
		worker, err := scanTeamWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("scan publication team roster: %w", err)
		}
		if worker.WorkspaceID != workspaceID || worker.TeamID != teamID {
			return nil, resolverError(CodeFrozenManifestMismatch, nil)
		}
		read.Workers = append(read.Workers, *worker)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read publication team roster: %w", err)
	}
	return &read, nil
}

// ListByWorker returns every team relation that references one worker agent.
func (r *TeamWorkerRepository) ListByWorker(
	ctx context.Context,
	workspaceID, workerAgentID string,
) ([]TeamWorker, error) {
	return r.list(ctx, `
		SELECT workspace_id, team_id, worker_agent_id, duty, when_to_use,
		       context_instruction, allowed_kinds, default_kind, result_requirement,
		       enabled, created_at, updated_at
		FROM weave_team_workers
		WHERE workspace_id=$1 AND worker_agent_id=$2
		ORDER BY team_id
	`, workspaceID, workerAgentID)
}

// ListByWorkspace returns every team-worker relation in one workspace,
// including disabled relations.
func (r *TeamWorkerRepository) ListByWorkspace(
	ctx context.Context,
	workspaceID string,
) ([]TeamWorker, error) {
	return r.list(ctx, `
		SELECT workspace_id, team_id, worker_agent_id, duty, when_to_use,
		       context_instruction, allowed_kinds, default_kind, result_requirement,
		       enabled, created_at, updated_at
		FROM weave_team_workers
		WHERE workspace_id=$1
		ORDER BY team_id, worker_agent_id
	`, workspaceID)
}

// ListTeamWorkers exposes the workspace roster to API read models without
// making legacy manages links a second team-membership source.
func (r *AgentRegistry) ListTeamWorkers(
	ctx context.Context,
	workspaceID string,
) ([]TeamWorker, error) {
	return NewTeamWorkerRepository(r.pool).ListByWorkspace(ctx, workspaceID)
}

// ResolveTeamWorkerDispatchRules resolves rules from the same active-team
// context as ListManaged. teamContext distinguishes an active team with no
// configured rule row from a legacy non-team caller.
func (r *AgentRegistry) ResolveTeamWorkerDispatchRules(
	ctx context.Context,
	workspaceID, leadName string,
) (rules TeamWorkerDispatchRules, teamContext, configured bool, err error) {
	var (
		teamStatus        string
		authoritativeLead bool
		legTimeout        *int
		groupDeadline     *int
		quorum            *int
	)
	err = r.pool.QueryRow(ctx, `
		SELECT team.id, team.status,
		       COALESCE(team.lead_avatar_id=lead.id, false),
		       rule.leg_timeout_sec, rule.group_deadline_sec, rule.quorum
		FROM weave_agents AS lead
		JOIN weave_teams AS team
		  ON team.workspace_id=lead.workspace_id
		 AND (
			team.lead_avatar_id=lead.id OR
			(team.lead_avatar_id IS NULL AND team.id=lead.team_id)
		 )
		LEFT JOIN weave_team_dispatch_rules AS rule
		  ON rule.team_id=team.id
		 AND team.status='active'
		 AND team.lead_avatar_id=lead.id
		WHERE lead.workspace_id=$1 AND lead.name=$2 AND lead.deleted=false
		ORDER BY CASE
			WHEN team.status='active' AND team.lead_avatar_id=lead.id THEN 0
			ELSE 1
		END, team.id
		LIMIT 1
	`, workspaceID, leadName).Scan(
		&rules.TeamID, &teamStatus, &authoritativeLead,
		&legTimeout, &groupDeadline, &quorum,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamWorkerDispatchRules{}, false, false, nil
	}
	if err != nil {
		return TeamWorkerDispatchRules{}, false, false, err
	}
	if teamStatus != "active" || !authoritativeLead {
		return rules, true, false, nil
	}
	if legTimeout == nil || groupDeadline == nil || quorum == nil {
		return rules, true, false, nil
	}
	rules.LegTimeoutSec = *legTimeout
	rules.GroupDeadlineSec = *groupDeadline
	rules.Quorum = *quorum
	return rules, true, true, nil
}

func (r *TeamWorkerRepository) list(
	ctx context.Context,
	query string,
	args ...any,
) ([]TeamWorker, error) {
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	workers := make([]TeamWorker, 0)
	for rows.Next() {
		worker, err := scanTeamWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, *worker)
	}
	return workers, rows.Err()
}

func scanTeamWorker(row rowScanner) (*TeamWorker, error) {
	var worker TeamWorker
	err := row.Scan(
		&worker.WorkspaceID,
		&worker.TeamID,
		&worker.WorkerAgentID,
		&worker.Duty,
		&worker.WhenToUse,
		&worker.ContextInstruction,
		&worker.AllowedKinds,
		&worker.DefaultKind,
		&worker.ResultRequirement,
		&worker.Enabled,
		&worker.CreatedAt,
		&worker.UpdatedAt,
	)
	return &worker, err
}
