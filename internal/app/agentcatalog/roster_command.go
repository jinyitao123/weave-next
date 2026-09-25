package agentcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jinyitao123/weave/internal/kernel/registry"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ApplyTeamRosterCommand applies one idempotent control-plane command and owns
// the transaction used to commit the frozen receipt and optional audit.
func (r *AgentRegistry) ApplyTeamRosterCommand(
	ctx context.Context,
	command registry.TeamRosterCommand,
) (*registry.TeamRosterResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin team roster command: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := r.ApplyTeamRosterCommandTx(ctx, tx, command)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit team roster command: %w", err)
	}
	return result, nil
}

// ApplyTeamBuildRollbackRosterTx applies the baseline-only roster command used
// by the team-build rollback sequence inside a caller-owned transaction. It
// enables the internal AllowRemove bit (departed workers are deleted) and
// allows restoring an archived team back to active. It never commits or rolls
// back the supplied transaction.
func (r *AgentRegistry) ApplyTeamBuildRollbackRosterTx(
	ctx context.Context,
	tx pgx.Tx,
	command registry.TeamRosterCommand,
) (*registry.TeamRosterResult, error) {
	return r.applyTeamRosterCommandTx(ctx, tx, command, true)
}

// ApplyTeamRosterCommandTx applies a command inside a caller-owned
// transaction. It never commits or rolls back the supplied transaction.
func (r *AgentRegistry) ApplyTeamRosterCommandTx(
	ctx context.Context,
	tx pgx.Tx,
	command registry.TeamRosterCommand,
) (*registry.TeamRosterResult, error) {
	return r.applyTeamRosterCommandTx(ctx, tx, command, false)
}

// applyTeamRosterCommandTx keeps rollback authority inside the product adapter.
// Callers cannot enable it through the serialized command contract.
func (r *AgentRegistry) applyTeamRosterCommandTx(ctx context.Context, tx pgx.Tx, command registry.TeamRosterCommand, rollback bool) (*registry.TeamRosterResult, error) {
	if tx == nil {
		return nil, fmt.Errorf("%w: transaction is required", registry.ErrTeamRosterInvalidRequest)
	}
	normalized, requestHash, err := teamRosterCommandHash(command)
	if err != nil {
		return nil, err
	}
	if err := validateRosterCommandIdentities(normalized); err != nil {
		return nil, err
	}

	team, err := lockRosterTeamForMutation(ctx, tx, normalized.WorkspaceID, normalized.TeamID)
	if errors.Is(err, registry.ErrTeamWorkerNotFound) {
		return nil, fmt.Errorf("%w: team %q", registry.ErrTeamRosterNotFound, normalized.TeamID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock team roster: %w", err)
	}

	storedHash, storedResponse, found, err := readTeamRosterReceipt(
		ctx, tx, normalized.WorkspaceID, normalized.TeamID, normalized.IdempotencyKey,
	)
	if err != nil {
		return nil, err
	}
	if found {
		if storedHash != requestHash {
			return nil, fmt.Errorf(
				"%w: key %q", registry.ErrTeamRosterIdempotencyConflict, normalized.IdempotencyKey,
			)
		}
		result, err := decodeTeamRosterResult(storedResponse)
		if err != nil {
			return nil, fmt.Errorf("decode team roster receipt: %w", err)
		}
		if result.TeamID != normalized.TeamID {
			return nil, errors.New("decode team roster receipt: frozen team identity mismatch")
		}
		return result, nil
	}

	if team.Status == "archived" && !rollback {
		return nil, fmt.Errorf("%w: team %q", registry.ErrTeamRosterArchived, normalized.TeamID)
	}
	if !team.UpdatedAt.UTC().Truncate(time.Microsecond).Equal(normalized.ExpectedUpdatedAt) {
		return nil, fmt.Errorf("%w: team %q", registry.ErrTeamRosterWriteConflict, normalized.TeamID)
	}

	managerID, err := findLegacyRosterManager(
		ctx, tx, normalized.WorkspaceID, normalized.TeamID, team.LeadAvatarID,
	)
	if err != nil {
		return nil, err
	}
	if err := lockAndValidateRosterAgents(
		ctx, tx, normalized.WorkspaceID, normalized.LeadAgentID, normalized.Workers, managerID,
	); err != nil {
		return nil, err
	}
	current, err := readLockedRosterWorkers(ctx, tx, normalized.WorkspaceID, normalized.TeamID)
	if err != nil {
		return nil, err
	}
	outcome, err := applyTeamRosterMutationTx(ctx, tx, teamRosterMutationRequest{
		WorkspaceID: normalized.WorkspaceID,
		TeamID:      normalized.TeamID,
		Team:        team,
		LeadAgentID: normalized.LeadAgentID,
		Status:      normalized.DesiredTeamStatus,
		Workers:     normalized.Workers,
		Current:     current,
		ManagerID:   managerID,
		AllowRemove: rollback,
	})
	if err != nil {
		return nil, err
	}

	counts, err := countAffectedPublishedVersionsTx(
		ctx, tx, normalized.WorkspaceID, normalized.TeamID, outcome.AffectedWorkerIDs,
	)
	if err != nil {
		return nil, err
	}
	result := teamRosterResultFromOutcome(normalized, outcome, counts)
	response, err := encodeTeamRosterResult(result)
	if err != nil {
		return nil, fmt.Errorf("encode team roster response: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_roster_receipts (
			workspace_id, team_id, idempotency_key, request_hash, response
		) VALUES ($1, $2, $3, $4, $5)
	`, normalized.WorkspaceID, normalized.TeamID, normalized.IdempotencyKey, requestHash, string(response)); err != nil {
		return nil, fmt.Errorf("insert team roster receipt: %w", err)
	}
	if outcome.Changed {
		if err := insertTeamRosterAudit(ctx, tx, normalized, outcome, *result.AuditID); err != nil {
			return nil, err
		}
	}
	if err := verifyTeamRosterMutation(
		ctx, tx, normalized.WorkspaceID, normalized.TeamID, outcome,
	); err != nil {
		return nil, err
	}
	return &result, nil
}

type lockedRosterTeam struct {
	Status       string
	LeadAvatarID *string
	UpdatedAt    time.Time
}

type teamRosterMutationRequest struct {
	WorkspaceID string
	TeamID      string
	Team        lockedRosterTeam
	LeadAgentID string
	Status      string
	Workers     []registry.TeamRosterWorkerInput
	Current     []registry.TeamRosterWorkerInput
	ManagerID   string
	AllowRemove bool
}

type teamRosterMutationOutcome struct {
	OldStatus         string
	NewStatus         string
	OldLeadAgentID    *string
	NewLeadAgentID    string
	OldWorkers        []registry.TeamRosterWorkerInput
	NewWorkers        []registry.TeamRosterWorkerInput
	AffectedWorkerIDs []string
	Changed           bool
	UpdatedAt         time.Time
}

func validateRosterCommandIdentities(command registry.TeamRosterCommand) error {
	if command.LeadAgentID != strings.TrimSpace(command.LeadAgentID) {
		return fmt.Errorf("%w: lead_agent_id must be trimmed", registry.ErrTeamRosterInvalidRequest)
	}
	for _, worker := range command.Workers {
		if worker.WorkerAgentID != strings.TrimSpace(worker.WorkerAgentID) {
			return fmt.Errorf(
				"%w: worker_agent_id must be trimmed", registry.ErrTeamRosterInvalidRequest,
			)
		}
	}
	return nil
}

func lockRosterTeamForMutation(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) (lockedRosterTeam, error) {
	var lockedWorkspaceID string
	err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedRosterTeam{}, fmt.Errorf(
			"%w: team %q in workspace %q", registry.ErrTeamWorkerNotFound, teamID, workspaceID,
		)
	}
	if err != nil {
		return lockedRosterTeam{}, err
	}

	var team lockedRosterTeam
	err = tx.QueryRow(ctx, `
		SELECT status, lead_avatar_id, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
		FOR UPDATE
	`, workspaceID, teamID).Scan(&team.Status, &team.LeadAvatarID, &team.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return lockedRosterTeam{}, fmt.Errorf(
			"%w: team %q in workspace %q", registry.ErrTeamWorkerNotFound, teamID, workspaceID,
		)
	}
	return team, err
}

func readTeamRosterReceipt(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID, idempotencyKey string,
) (string, []byte, bool, error) {
	var (
		requestHash string
		response    []byte
	)
	err := tx.QueryRow(ctx, `
		SELECT request_hash, response
		FROM weave_team_roster_receipts
		WHERE workspace_id=$1 AND team_id=$2 AND idempotency_key=$3
	`, workspaceID, teamID, idempotencyKey).Scan(&requestHash, &response)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("read team roster receipt: %w", err)
	}
	return requestHash, response, true, nil
}

func findLegacyRosterManager(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
	leadAgentID *string,
) (string, error) {
	if leadAgentID != nil && *leadAgentID != "" {
		return *leadAgentID, nil
	}
	var managerID string
	err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_agents
		WHERE workspace_id=$1 AND team_id=$2 AND role='avatar' AND deleted=false
		ORDER BY id COLLATE "C"
		LIMIT 1
	`, workspaceID, teamID).Scan(&managerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find current team lead: %w", err)
	}
	return managerID, nil
}

func lockAndValidateRosterAgents(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, leadAgentID string,
	workers []registry.TeamRosterWorkerInput,
	extraAgentID string,
) error {
	if leadAgentID == "" {
		return fmt.Errorf("%w: team lead is required", registry.ErrActiveTeamRequiresEnabledWorker)
	}
	seen := make(map[string]struct{}, len(workers)+2)
	seen[leadAgentID] = struct{}{}
	agentIDs := []string{leadAgentID}
	for _, worker := range workers {
		if _, duplicate := seen[worker.WorkerAgentID]; duplicate {
			return fmt.Errorf("agent %q appears more than once in roster", worker.WorkerAgentID)
		}
		seen[worker.WorkerAgentID] = struct{}{}
		agentIDs = append(agentIDs, worker.WorkerAgentID)
	}
	if extraAgentID != "" {
		if _, exists := seen[extraAgentID]; !exists {
			seen[extraAgentID] = struct{}{}
			agentIDs = append(agentIDs, extraAgentID)
		}
	}
	sort.Strings(agentIDs)

	type rosterAgent struct {
		role    string
		deleted bool
		spec    []byte
	}
	rows, err := tx.Query(ctx, `
		SELECT id, role, deleted, spec
		FROM weave_agents
		WHERE workspace_id=$1 AND id=ANY($2::text[])
		ORDER BY id COLLATE "C"
		FOR UPDATE
	`, workspaceID, agentIDs)
	if err != nil {
		return fmt.Errorf("lock roster agents: %w", err)
	}
	agents := make(map[string]rosterAgent, len(agentIDs))
	for rows.Next() {
		var agentID string
		var agent rosterAgent
		if err := rows.Scan(&agentID, &agent.role, &agent.deleted, &agent.spec); err != nil {
			rows.Close()
			return fmt.Errorf("scan roster agent: %w", err)
		}
		agents[agentID] = agent
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("lock roster agents: %w", err)
	}
	rows.Close()

	lead, exists := agents[leadAgentID]
	if !exists || lead.deleted {
		return fmt.Errorf("active agent %q not found in workspace %q", leadAgentID, workspaceID)
	}
	if lead.role != "avatar" {
		return fmt.Errorf("team lead %q must have avatar role", leadAgentID)
	}
	for _, worker := range workers {
		agent, exists := agents[worker.WorkerAgentID]
		if !exists || agent.deleted {
			return fmt.Errorf(
				"active agent %q not found in workspace %q", worker.WorkerAgentID, workspaceID,
			)
		}
		if agent.role != "worker" {
			return fmt.Errorf("team worker %q must have worker role", worker.WorkerAgentID)
		}
		var record registry.AgentRecord
		if err := json.Unmarshal(agent.spec, &record); err != nil {
			return fmt.Errorf("decode roster agent %q: %w", worker.WorkerAgentID, err)
		}
		if err := registry.ValidateTeamWorkerAgentRecord(&record); err != nil {
			return err
		}
	}
	return nil
}

func readLockedRosterWorkers(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) ([]registry.TeamRosterWorkerInput, error) {
	return readRosterWorkers(ctx, tx, workspaceID, teamID, true)
}

func readTeamRosterWorkers(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
) ([]registry.TeamRosterWorkerInput, error) {
	return readRosterWorkers(ctx, tx, workspaceID, teamID, false)
}

func readRosterWorkers(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
	lock bool,
) ([]registry.TeamRosterWorkerInput, error) {
	lockClause := ""
	if lock {
		lockClause = " FOR UPDATE"
	}
	rows, err := tx.Query(ctx, `
		SELECT workspace_id, team_id, worker_agent_id, duty, when_to_use,
		       context_instruction, allowed_kinds, default_kind, result_requirement,
		       enabled, created_at, updated_at
		FROM weave_team_workers
		WHERE workspace_id=$1 AND team_id=$2
		ORDER BY worker_agent_id COLLATE "C"`+lockClause,
		workspaceID, teamID,
	)
	if err != nil {
		return nil, fmt.Errorf("read team roster: %w", err)
	}
	defer rows.Close()
	workers := make([]registry.TeamRosterWorkerInput, 0)
	for rows.Next() {
		worker, err := scanTeamWorker(rows)
		if err != nil {
			return nil, fmt.Errorf("scan team roster: %w", err)
		}
		workers = append(workers, teamRosterWorkerInput(*worker))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read team roster: %w", err)
	}
	return workers, nil
}

func teamRosterWorkerInput(worker registry.TeamWorker) registry.TeamRosterWorkerInput {
	return registry.TeamRosterWorkerInput{
		WorkerAgentID:      worker.WorkerAgentID,
		Duty:               worker.Duty,
		WhenToUse:          worker.WhenToUse,
		ContextInstruction: worker.ContextInstruction,
		AllowedKinds:       append([]string(nil), worker.AllowedKinds...),
		DefaultKind:        worker.DefaultKind,
		ResultRequirement:  worker.ResultRequirement,
		Enabled:            worker.Enabled,
	}
}

func applyTeamRosterMutationTx(
	ctx context.Context,
	tx pgx.Tx,
	request teamRosterMutationRequest,
) (teamRosterMutationOutcome, error) {
	if err := validateRosterFinalState(request); err != nil {
		return teamRosterMutationOutcome{}, err
	}
	outcome := teamRosterMutationOutcome{
		OldStatus:         request.Team.Status,
		NewStatus:         request.Status,
		OldLeadAgentID:    cloneStringPointer(request.Team.LeadAvatarID),
		NewLeadAgentID:    request.LeadAgentID,
		OldWorkers:        cloneTeamRosterWorkers(request.Current),
		NewWorkers:        cloneTeamRosterWorkers(request.Workers),
		AffectedWorkerIDs: changedAuthorizationWorkerIDs(request.Current, request.Workers),
		UpdatedAt:         request.Team.UpdatedAt,
	}
	outcome.Changed = request.Team.Status != request.Status ||
		!stringPointerEquals(request.Team.LeadAvatarID, request.LeadAgentID) ||
		!reflect.DeepEqual(request.Current, request.Workers)
	if !outcome.Changed {
		return outcome, nil
	}

	for _, worker := range request.Workers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO weave_team_workers (
				workspace_id, team_id, worker_agent_id, duty, when_to_use,
				context_instruction, allowed_kinds, default_kind,
				result_requirement, enabled
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (team_id, worker_agent_id) DO UPDATE SET
				duty=EXCLUDED.duty,
				when_to_use=EXCLUDED.when_to_use,
				context_instruction=EXCLUDED.context_instruction,
				allowed_kinds=EXCLUDED.allowed_kinds,
				default_kind=EXCLUDED.default_kind,
				result_requirement=EXCLUDED.result_requirement,
				enabled=EXCLUDED.enabled,
				updated_at=statement_timestamp()
			WHERE weave_team_workers.workspace_id=EXCLUDED.workspace_id
			  AND (
				weave_team_workers.duty IS DISTINCT FROM EXCLUDED.duty OR
				weave_team_workers.when_to_use IS DISTINCT FROM EXCLUDED.when_to_use OR
				weave_team_workers.context_instruction IS DISTINCT FROM EXCLUDED.context_instruction OR
				weave_team_workers.allowed_kinds IS DISTINCT FROM EXCLUDED.allowed_kinds OR
				weave_team_workers.default_kind IS DISTINCT FROM EXCLUDED.default_kind OR
				weave_team_workers.result_requirement IS DISTINCT FROM EXCLUDED.result_requirement OR
				weave_team_workers.enabled IS DISTINCT FROM EXCLUDED.enabled
			  )
		`, request.WorkspaceID, request.TeamID, worker.WorkerAgentID, worker.Duty,
			worker.WhenToUse, worker.ContextInstruction, worker.AllowedKinds,
			worker.DefaultKind, worker.ResultRequirement, worker.Enabled); err != nil {
			return teamRosterMutationOutcome{}, fmt.Errorf(
				"upsert team worker %q: %w", worker.WorkerAgentID, err,
			)
		}
	}

	if request.AllowRemove {
		workerIDs := make([]string, 0, len(request.Workers))
		for _, worker := range request.Workers {
			workerIDs = append(workerIDs, worker.WorkerAgentID)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM weave_team_workers
			WHERE workspace_id=$1 AND team_id=$2
			  AND NOT (worker_agent_id=ANY($3::text[]))
		`, request.WorkspaceID, request.TeamID, workerIDs); err != nil {
			return teamRosterMutationOutcome{}, fmt.Errorf("remove departed team workers: %w", err)
		}
	}

	managerIDs := uniqueSortedStrings(request.ManagerID, request.LeadAgentID)
	if len(managerIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			DELETE FROM weave_agent_links
			WHERE workspace_id=$1 AND type='manages'
			  AND from_agent_id=ANY($2::text[])
		`, request.WorkspaceID, managerIDs); err != nil {
			return teamRosterMutationOutcome{}, fmt.Errorf(
				"remove legacy team manages links: %w", err,
			)
		}
	}

	if err := ensureActiveTeamHasEnabledWorker(
		ctx, tx, request.WorkspaceID, request.TeamID, request.Status,
	); err != nil {
		return teamRosterMutationOutcome{}, err
	}
	if request.Status == "archived" {
		if err := ensureRosterTeamArchiveAllowed(ctx, tx, request.WorkspaceID, request.TeamID); err != nil {
			return teamRosterMutationOutcome{}, err
		}
	}
	if err := tx.QueryRow(ctx, `
		UPDATE weave_teams
		SET lead_avatar_id=$3, status=$4
		WHERE workspace_id=$1 AND id=$2
		RETURNING updated_at
	`, request.WorkspaceID, request.TeamID, request.LeadAgentID, request.Status).Scan(
		&outcome.UpdatedAt,
	); err != nil {
		return teamRosterMutationOutcome{}, fmt.Errorf("update team roster state: %w", err)
	}
	if request.Status == "active" {
		if _, err := tx.Exec(ctx, `
			UPDATE weave_projects
			SET avatar_id=$3
			WHERE workspace_id=$1 AND team_id=$2
			  AND archived_at IS NULL
			  AND COALESCE(system_kind,'') <> 'unclassified'
		`, request.WorkspaceID, request.TeamID, request.LeadAgentID); err != nil {
			return teamRosterMutationOutcome{}, fmt.Errorf("sync project lead mirror: %w", err)
		}
	}
	return outcome, nil
}

func ensureRosterTeamArchiveAllowed(ctx context.Context, tx pgx.Tx, workspaceID, teamID string) error {
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
		return fmt.Errorf("%w: archive blockers=%s", registry.ErrTeamRosterInvalidRequest, strings.Join(blockers, ","))
	}
	return nil
}

func validateRosterFinalState(request teamRosterMutationRequest) error {
	if request.LeadAgentID == "" || len(request.Workers) == 0 {
		return fmt.Errorf("%w: team %q", registry.ErrActiveTeamRequiresEnabledWorker, request.TeamID)
	}
	if !request.AllowRemove {
		finalIDs := make(map[string]struct{}, len(request.Workers))
		for _, worker := range request.Workers {
			finalIDs[worker.WorkerAgentID] = struct{}{}
		}
		for _, worker := range request.Current {
			if _, retained := finalIDs[worker.WorkerAgentID]; !retained {
				return fmt.Errorf(
					"%w: worker %q", registry.ErrTeamRosterRemovalUnsupported, worker.WorkerAgentID,
				)
			}
		}
	}
	if request.Status == "archived" {
		if request.Team.LeadAvatarID == nil || *request.Team.LeadAvatarID == "" ||
			*request.Team.LeadAvatarID != request.LeadAgentID {
			return fmt.Errorf(
				"%w: archive must preserve the existing lead", registry.ErrTeamRosterInvalidRequest,
			)
		}
		return nil
	}
	if request.Status != "active" && request.Status != "building" {
		return fmt.Errorf(
			"%w: invalid final team status %q", registry.ErrTeamRosterInvalidRequest, request.Status,
		)
	}
	for _, worker := range request.Workers {
		if worker.Enabled {
			return nil
		}
	}
	return fmt.Errorf("%w: team %q", registry.ErrActiveTeamRequiresEnabledWorker, request.TeamID)
}

func changedAuthorizationWorkerIDs(
	oldWorkers, newWorkers []registry.TeamRosterWorkerInput,
) []string {
	oldByID := make(map[string]registry.TeamRosterWorkerInput, len(oldWorkers))
	newByID := make(map[string]registry.TeamRosterWorkerInput, len(newWorkers))
	ids := make(map[string]struct{}, len(oldWorkers)+len(newWorkers))
	for _, worker := range oldWorkers {
		oldByID[worker.WorkerAgentID] = worker
		ids[worker.WorkerAgentID] = struct{}{}
	}
	for _, worker := range newWorkers {
		newByID[worker.WorkerAgentID] = worker
		ids[worker.WorkerAgentID] = struct{}{}
	}
	changed := make([]string, 0)
	for id := range ids {
		oldWorker, oldExists := oldByID[id]
		newWorker, newExists := newByID[id]
		if !oldExists || !newExists || oldWorker.DefaultKind != newWorker.DefaultKind ||
			oldWorker.Enabled != newWorker.Enabled ||
			!equalKindSets(oldWorker.AllowedKinds, newWorker.AllowedKinds) {
			changed = append(changed, id)
		}
	}
	sort.Strings(changed)
	return changed
}

func equalKindSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	sortKinds(leftCopy)
	sortKinds(rightCopy)
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func teamRosterResultFromOutcome(
	command registry.TeamRosterCommand,
	outcome teamRosterMutationOutcome,
	counts map[string]int,
) registry.TeamRosterResult {
	result := registry.TeamRosterResult{
		SchemaVersion: teamRosterResultSchemaVersion,
		TeamID:        command.TeamID,
		TeamStatus:    outcome.NewStatus,
		LeadAgentID:   outcome.NewLeadAgentID,
		UpdatedAt:     formatTeamRosterTime(outcome.UpdatedAt),
		Workers:       cloneTeamRosterWorkers(outcome.NewWorkers),
		Changed:       outcome.Changed,
	}
	if !outcome.Changed {
		result.AffectedWorkers = []registry.TeamRosterAffectedWorker{}
		return result
	}
	auditID := uuid.NewString()
	result.AuditID = &auditID
	result.AffectedWorkers = make(
		[]registry.TeamRosterAffectedWorker, 0, len(outcome.AffectedWorkerIDs),
	)
	for _, workerID := range outcome.AffectedWorkerIDs {
		result.AffectedWorkers = append(result.AffectedWorkers, registry.TeamRosterAffectedWorker{
			WorkerAgentID:                 workerID,
			AffectedPublishedVersionCount: counts[workerID],
			RevocationImpactURL: fmt.Sprintf(
				"/v1/teams/%s/workers/%s/revocation-impact",
				url.PathEscape(command.TeamID),
				url.PathEscape(workerID),
			),
		})
	}
	return result
}

func insertTeamRosterAudit(
	ctx context.Context,
	tx pgx.Tx,
	command registry.TeamRosterCommand,
	outcome teamRosterMutationOutcome,
	auditID string,
) error {
	oldWorkers, err := json.Marshal(teamRosterAuthorizationFacts(outcome.OldWorkers))
	if err != nil {
		return fmt.Errorf("encode old team roster audit facts: %w", err)
	}
	newWorkers, err := json.Marshal(teamRosterAuthorizationFacts(outcome.NewWorkers))
	if err != nil {
		return fmt.Errorf("encode new team roster audit facts: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO weave_team_roster_audits (
			workspace_id, team_id, audit_id, idempotency_key,
			old_team_status, new_team_status, old_lead_agent_id,
			new_lead_agent_id, old_workers, new_workers, operator_id, reason
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, command.WorkspaceID, command.TeamID, auditID, command.IdempotencyKey,
		outcome.OldStatus, outcome.NewStatus, outcome.OldLeadAgentID,
		outcome.NewLeadAgentID, string(oldWorkers), string(newWorkers), command.OperatorID, command.Reason,
	); err != nil {
		return fmt.Errorf("insert team roster audit: %w", err)
	}
	return nil
}

func teamRosterAuthorizationFacts(
	workers []registry.TeamRosterWorkerInput,
) []registry.TeamRosterAuthorizationFact {
	facts := make([]registry.TeamRosterAuthorizationFact, 0, len(workers))
	for _, worker := range workers {
		kinds := append([]string(nil), worker.AllowedKinds...)
		sortKinds(kinds)
		facts = append(facts, registry.TeamRosterAuthorizationFact{
			WorkerAgentID: worker.WorkerAgentID,
			AllowedKinds:  kinds,
			DefaultKind:   worker.DefaultKind,
			Enabled:       worker.Enabled,
		})
	}
	sort.Slice(facts, func(i, j int) bool {
		return facts[i].WorkerAgentID < facts[j].WorkerAgentID
	})
	return facts
}

func verifyTeamRosterMutation(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID, teamID string,
	outcome teamRosterMutationOutcome,
) error {
	var (
		status    string
		lead      *string
		updatedAt time.Time
	)
	if err := tx.QueryRow(ctx, `
		SELECT status, lead_avatar_id, updated_at
		FROM weave_teams
		WHERE workspace_id=$1 AND id=$2
	`, workspaceID, teamID).Scan(&status, &lead, &updatedAt); err != nil {
		return fmt.Errorf("reread persisted team roster state: %w", err)
	}
	workers, err := readTeamRosterWorkers(ctx, tx, workspaceID, teamID)
	if err != nil {
		return err
	}
	if status != outcome.NewStatus || !stringPointerEquals(lead, outcome.NewLeadAgentID) ||
		!updatedAt.Equal(outcome.UpdatedAt) || !reflect.DeepEqual(workers, outcome.NewWorkers) {
		return errors.New("persisted team roster does not match command result")
	}
	return nil
}

func stringPointerEquals(value *string, expected string) bool {
	return value != nil && *value == expected
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func uniqueSortedStrings(values ...string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
