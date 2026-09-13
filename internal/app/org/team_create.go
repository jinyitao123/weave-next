package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jinyitao123/weave/internal/kernel/orgspec"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// CreateActiveTeam atomically creates an active Team, its lead relation, and
// an enabled TeamWorker row for every configured initial worker.
func (s *Store) CreateActiveTeam(
	ctx context.Context,
	workspaceID string,
	input orgspec.CreateActiveTeamInput,
) (orgspec.CreateActiveTeamResult, error) {
	if err := validateCreateActiveTeamInput(workspaceID, input); err != nil {
		return orgspec.CreateActiveTeamResult{}, err
	}

	team := orgspec.Team{
		ID:              uuid.NewString(),
		WorkspaceID:     workspaceID,
		Name:            input.Name,
		DisplayName:     strings.TrimSpace(input.DisplayName),
		Objective:       input.Objective,
		PrimaryScenario: input.PrimaryScenario,
		SuccessCriteria: input.SuccessCriteria,
		LeadAvatarID:    input.LeadAvatarID,
		Status:          finalCreateTeamStatus(input.DesiredStatus),
		Evaluation:      finalCreateTeamEvaluation(input.Evaluation),
	}
	if team.DisplayName == "" {
		team.DisplayName = team.Name
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("begin active team creation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedWorkspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID); err != nil {
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("lock team workspace %q: %w", workspaceID, err)
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO weave_teams (
			id, workspace_id, name, display_name, objective, primary_scenario,
			success_criteria, lead_avatar_id, status, evaluation
		) VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, 'needs_repair', $8)
		RETURNING created_at, updated_at
	`,
		team.ID, team.WorkspaceID, team.Name, team.DisplayName, team.Objective,
		team.PrimaryScenario, team.SuccessCriteria, team.Evaluation,
	).Scan(&team.CreatedAt, &team.UpdatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "weave_teams_workspace_id_name_key" {
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("%w: %q", orgspec.ErrTeamNameConflict, input.Name)
		}
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("insert team pending validation: %w", err)
	}

	agentIDs := make([]string, 0, len(input.Workers)+1)
	agentIDs = append(agentIDs, input.LeadAvatarID)
	for _, worker := range input.Workers {
		agentIDs = append(agentIDs, worker.WorkerAgentID)
	}
	sort.Strings(agentIDs)

	lockedAgents := make(map[string]teamAgentSnapshot, len(agentIDs))
	rows, err := tx.Query(ctx, `
		SELECT id, workspace_id, role, deleted, spec
		FROM weave_agents
		WHERE workspace_id=$1 AND id=ANY($2::text[])
		ORDER BY id
		FOR UPDATE
	`, workspaceID, agentIDs)
	if err != nil {
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("lock active team agents: %w", err)
	}
	for rows.Next() {
		var id string
		var agent teamAgentSnapshot
		var data []byte
		if err := rows.Scan(&id, &agent.workspaceID, &agent.role, &agent.deleted, &data); err != nil {
			rows.Close()
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("scan active team agent: %w", err)
		}
		if err := json.Unmarshal(data, &agent.record); err != nil {
			rows.Close()
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("decode active team agent %q: %w", id, err)
		}
		lockedAgents[id] = agent
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("lock active team agents: %w", err)
	}
	rows.Close()

	lead, ok := lockedAgents[input.LeadAvatarID]
	if !teamLeadAvailable(workspaceID, lead, ok) {
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("%w: %q", orgspec.ErrTeamLeadUnavailable, input.LeadAvatarID)
	}
	for _, worker := range input.Workers {
		agent, ok := lockedAgents[worker.WorkerAgentID]
		if !teamWorkerAvailable(workspaceID, agent, ok) {
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("%w: %q", orgspec.ErrTeamWorkerUnavailable, worker.WorkerAgentID)
		}
		if err := teamWorkerRecordCompatible(&agent.record); err != nil {
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("%w: %q: %v", orgspec.ErrTeamWorkerUnavailable, worker.WorkerAgentID, err)
		}
	}

	leadConflict, err := teamLeadConflict(ctx, tx, workspaceID, input.LeadAvatarID)
	if err != nil {
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("check active team lead: %w", err)
	}
	if leadConflict {
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("%w: %q", orgspec.ErrTeamLeadConflict, input.LeadAvatarID)
	}

	result := orgspec.CreateActiveTeamResult{
		Team:    team,
		Workers: make([]orgspec.TeamWorker, 0, len(input.Workers)),
	}
	for _, initial := range input.Workers {
		worker := orgspec.TeamWorker{
			WorkspaceID:        workspaceID,
			TeamID:             team.ID,
			WorkerAgentID:      initial.WorkerAgentID,
			Duty:               initial.Duty,
			WhenToUse:          initial.WhenToUse,
			ContextInstruction: initial.ContextInstruction,
			AllowedKinds:       append([]string(nil), initial.AllowedKinds...),
			DefaultKind:        initial.DefaultKind,
			ResultRequirement:  initial.ResultRequirement,
			Enabled:            true,
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO weave_team_workers (
				workspace_id, team_id, worker_agent_id, duty, when_to_use,
				context_instruction, allowed_kinds, default_kind,
				result_requirement, enabled
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true)
			RETURNING created_at, updated_at
		`,
			worker.WorkspaceID, worker.TeamID, worker.WorkerAgentID,
			worker.Duty, worker.WhenToUse, worker.ContextInstruction,
			worker.AllowedKinds, worker.DefaultKind, worker.ResultRequirement,
		).Scan(&worker.CreatedAt, &worker.UpdatedAt); err != nil {
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("insert team worker %q: %w", worker.WorkerAgentID, err)
		}
		result.Workers = append(result.Workers, worker)
	}

	err = tx.QueryRow(ctx, `
		UPDATE weave_teams
		SET lead_avatar_id=$1, status=$4, updated_at=now()
		WHERE workspace_id=$2 AND id=$3 AND status='needs_repair'
		RETURNING status, updated_at
	`, input.LeadAvatarID, workspaceID, team.ID, team.Status).Scan(&result.Team.Status, &result.Team.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "uniq_weave_active_team_lead_avatar" {
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("%w: %q", orgspec.ErrTeamLeadConflict, input.LeadAvatarID)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return orgspec.CreateActiveTeamResult{}, fmt.Errorf("activate team %q: row changed", team.ID)
		}
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("activate team %q: %w", team.ID, err)
	}
	result.Team.LeadAvatarID = input.LeadAvatarID

	if err := tx.Commit(ctx); err != nil {
		return orgspec.CreateActiveTeamResult{}, fmt.Errorf("commit active team creation: %w", err)
	}
	return result, nil
}

func validateCreateActiveTeamInput(workspaceID string, input orgspec.CreateActiveTeamInput) error {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(input.Name) == "" ||
		strings.TrimSpace(input.Objective) == "" || strings.TrimSpace(input.LeadAvatarID) == "" ||
		len(input.Workers) == 0 {
		return orgspec.ErrInvalidTeamCreationInput
	}
	if status := finalCreateTeamStatus(input.DesiredStatus); status != "active" && status != "building" {
		return orgspec.ErrInvalidTeamCreationInput
	}
	if evaluation := finalCreateTeamEvaluation(input.Evaluation); evaluation != orgspec.TeamEvaluationEvaluated && evaluation != orgspec.TeamEvaluationUnevaluated {
		return orgspec.ErrInvalidTeamCreationInput
	}

	workerIDs := make(map[string]struct{}, len(input.Workers))
	validKinds := map[string]struct{}{
		"consult":  {},
		"dispatch": {},
		"handoff":  {},
	}
	for index, worker := range input.Workers {
		if strings.TrimSpace(worker.WorkerAgentID) == "" || worker.WorkerAgentID == input.LeadAvatarID {
			return fmt.Errorf("%w: invalid worker at index %d", orgspec.ErrInvalidTeamCreationInput, index)
		}
		if _, duplicate := workerIDs[worker.WorkerAgentID]; duplicate {
			return fmt.Errorf("%w: duplicate worker %q", orgspec.ErrInvalidTeamCreationInput, worker.WorkerAgentID)
		}
		workerIDs[worker.WorkerAgentID] = struct{}{}

		allowed := make(map[string]struct{}, len(worker.AllowedKinds))
		for _, kind := range worker.AllowedKinds {
			if _, valid := validKinds[kind]; !valid {
				return fmt.Errorf("%w: worker %q has unknown kind %q", orgspec.ErrInvalidTeamWorkerKinds, worker.WorkerAgentID, kind)
			}
			if _, duplicate := allowed[kind]; duplicate {
				return fmt.Errorf("%w: worker %q repeats kind %q", orgspec.ErrInvalidTeamWorkerKinds, worker.WorkerAgentID, kind)
			}
			allowed[kind] = struct{}{}
		}
		if len(allowed) == 0 {
			return fmt.Errorf("%w: worker %q has no allowed kinds", orgspec.ErrInvalidTeamWorkerKinds, worker.WorkerAgentID)
		}
		if _, ok := allowed[worker.DefaultKind]; !ok {
			return fmt.Errorf("%w: worker %q default %q is not allowed", orgspec.ErrInvalidTeamWorkerKinds, worker.WorkerAgentID, worker.DefaultKind)
		}
	}
	return nil
}

func finalCreateTeamStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "active"
	}
	return status
}

func finalCreateTeamEvaluation(evaluation string) string {
	evaluation = strings.TrimSpace(evaluation)
	if evaluation == "" {
		return orgspec.TeamEvaluationEvaluated
	}
	return evaluation
}
