package org

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrInvalidTeamCreationInput indicates that required aggregate fields are
	// missing or that the initial worker list contains duplicate identities.
	ErrInvalidTeamCreationInput = errors.New("invalid active team creation input")
	// ErrInvalidTeamWorkerKinds indicates that a worker's allowed/default kind
	// configuration does not form a valid consult/dispatch/handoff set.
	ErrInvalidTeamWorkerKinds = errors.New("invalid team worker kinds")
	// ErrTeamLeadUnavailable indicates that the requested lead is not a live
	// avatar in the target workspace.
	ErrTeamLeadUnavailable = errors.New("team lead avatar is unavailable")
	// ErrTeamWorkerUnavailable indicates that a requested member is not a live
	// worker in the target workspace.
	ErrTeamWorkerUnavailable = errors.New("team worker is unavailable")
	// ErrTeamLeadConflict indicates that the avatar already leads an active team.
	ErrTeamLeadConflict = errors.New("team lead avatar already leads an active team")
	// ErrTeamNameConflict indicates that the workspace already contains a Team
	// with the requested name.
	ErrTeamNameConflict = errors.New("team name already exists in workspace")
)

// InitialTeamWorker describes one enabled worker created with a Team.
type InitialTeamWorker struct {
	WorkerAgentID      string   `json:"worker_agent_id"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
}

// CreateActiveTeamInput contains the complete aggregate required to create an
// immediately usable Team.
type CreateActiveTeamInput struct {
	Name            string              `json:"name"`
	Objective       string              `json:"objective"`
	PrimaryScenario string              `json:"primary_scenario"`
	SuccessCriteria string              `json:"success_criteria"`
	LeadAvatarID    string              `json:"lead_avatar_id"`
	Workers         []InitialTeamWorker `json:"workers"`
	DesiredStatus   string              `json:"-"`
	Evaluation      string              `json:"-"`
}

// TeamWorker is one workspace-scoped Team membership and its call policy.
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

// CreateActiveTeamResult is the Team aggregate committed by CreateActiveTeam.
type CreateActiveTeamResult struct {
	Team    Team         `json:"team"`
	Workers []TeamWorker `json:"workers"`
}

// CreateActiveTeam atomically creates an active Team, its lead relation, and
// an enabled TeamWorker row for every configured initial worker.
func (s *Store) CreateActiveTeam(
	ctx context.Context,
	workspaceID string,
	input CreateActiveTeamInput,
) (CreateActiveTeamResult, error) {
	if err := validateCreateActiveTeamInput(workspaceID, input); err != nil {
		return CreateActiveTeamResult{}, err
	}

	team := Team{
		ID:              uuid.NewString(),
		WorkspaceID:     workspaceID,
		Name:            input.Name,
		Objective:       input.Objective,
		PrimaryScenario: input.PrimaryScenario,
		SuccessCriteria: input.SuccessCriteria,
		LeadAvatarID:    input.LeadAvatarID,
		Status:          finalCreateTeamStatus(input.DesiredStatus),
		Evaluation:      finalCreateTeamEvaluation(input.Evaluation),
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreateActiveTeamResult{}, fmt.Errorf("begin active team creation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedWorkspaceID string
	if err := tx.QueryRow(ctx, `
		SELECT id
		FROM weave_workspaces
		WHERE id=$1
		FOR UPDATE
	`, workspaceID).Scan(&lockedWorkspaceID); err != nil {
		return CreateActiveTeamResult{}, fmt.Errorf("lock team workspace %q: %w", workspaceID, err)
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO weave_teams (
			id, workspace_id, name, objective, primary_scenario,
			success_criteria, lead_avatar_id, status, evaluation
		) VALUES ($1, $2, $3, $4, $5, $6, NULL, 'needs_repair', $7)
		RETURNING created_at, updated_at
	`,
		team.ID, team.WorkspaceID, team.Name, team.Objective,
		team.PrimaryScenario, team.SuccessCriteria, team.Evaluation,
	).Scan(&team.CreatedAt, &team.UpdatedAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "weave_teams_workspace_id_name_key" {
			return CreateActiveTeamResult{}, fmt.Errorf("%w: %q", ErrTeamNameConflict, input.Name)
		}
		return CreateActiveTeamResult{}, fmt.Errorf("insert team pending validation: %w", err)
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
		return CreateActiveTeamResult{}, fmt.Errorf("lock active team agents: %w", err)
	}
	for rows.Next() {
		var id string
		var agent teamAgentSnapshot
		var data []byte
		if err := rows.Scan(&id, &agent.workspaceID, &agent.role, &agent.deleted, &data); err != nil {
			rows.Close()
			return CreateActiveTeamResult{}, fmt.Errorf("scan active team agent: %w", err)
		}
		if err := json.Unmarshal(data, &agent.record); err != nil {
			rows.Close()
			return CreateActiveTeamResult{}, fmt.Errorf("decode active team agent %q: %w", id, err)
		}
		lockedAgents[id] = agent
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return CreateActiveTeamResult{}, fmt.Errorf("lock active team agents: %w", err)
	}
	rows.Close()

	lead, ok := lockedAgents[input.LeadAvatarID]
	if !teamLeadAvailable(workspaceID, lead, ok) {
		return CreateActiveTeamResult{}, fmt.Errorf("%w: %q", ErrTeamLeadUnavailable, input.LeadAvatarID)
	}
	for _, worker := range input.Workers {
		agent, ok := lockedAgents[worker.WorkerAgentID]
		if !teamWorkerAvailable(workspaceID, agent, ok) {
			return CreateActiveTeamResult{}, fmt.Errorf("%w: %q", ErrTeamWorkerUnavailable, worker.WorkerAgentID)
		}
		if err := teamWorkerRecordCompatible(&agent.record); err != nil {
			return CreateActiveTeamResult{}, fmt.Errorf("%w: %q: %v", ErrTeamWorkerUnavailable, worker.WorkerAgentID, err)
		}
	}

	leadConflict, err := teamLeadConflict(ctx, tx, workspaceID, input.LeadAvatarID)
	if err != nil {
		return CreateActiveTeamResult{}, fmt.Errorf("check active team lead: %w", err)
	}
	if leadConflict {
		return CreateActiveTeamResult{}, fmt.Errorf("%w: %q", ErrTeamLeadConflict, input.LeadAvatarID)
	}

	result := CreateActiveTeamResult{
		Team:    team,
		Workers: make([]TeamWorker, 0, len(input.Workers)),
	}
	for _, initial := range input.Workers {
		worker := TeamWorker{
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
			return CreateActiveTeamResult{}, fmt.Errorf("insert team worker %q: %w", worker.WorkerAgentID, err)
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
			return CreateActiveTeamResult{}, fmt.Errorf("%w: %q", ErrTeamLeadConflict, input.LeadAvatarID)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return CreateActiveTeamResult{}, fmt.Errorf("activate team %q: row changed", team.ID)
		}
		return CreateActiveTeamResult{}, fmt.Errorf("activate team %q: %w", team.ID, err)
	}
	result.Team.LeadAvatarID = input.LeadAvatarID

	if err := tx.Commit(ctx); err != nil {
		return CreateActiveTeamResult{}, fmt.Errorf("commit active team creation: %w", err)
	}
	return result, nil
}

func validateCreateActiveTeamInput(workspaceID string, input CreateActiveTeamInput) error {
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(input.Name) == "" ||
		strings.TrimSpace(input.Objective) == "" || strings.TrimSpace(input.LeadAvatarID) == "" ||
		len(input.Workers) == 0 {
		return ErrInvalidTeamCreationInput
	}
	if status := finalCreateTeamStatus(input.DesiredStatus); status != "active" && status != "building" {
		return ErrInvalidTeamCreationInput
	}
	if evaluation := finalCreateTeamEvaluation(input.Evaluation); evaluation != TeamEvaluationEvaluated && evaluation != TeamEvaluationUnevaluated {
		return ErrInvalidTeamCreationInput
	}

	workerIDs := make(map[string]struct{}, len(input.Workers))
	validKinds := map[string]struct{}{
		"consult":  {},
		"dispatch": {},
		"handoff":  {},
	}
	for index, worker := range input.Workers {
		if strings.TrimSpace(worker.WorkerAgentID) == "" || worker.WorkerAgentID == input.LeadAvatarID {
			return fmt.Errorf("%w: invalid worker at index %d", ErrInvalidTeamCreationInput, index)
		}
		if _, duplicate := workerIDs[worker.WorkerAgentID]; duplicate {
			return fmt.Errorf("%w: duplicate worker %q", ErrInvalidTeamCreationInput, worker.WorkerAgentID)
		}
		workerIDs[worker.WorkerAgentID] = struct{}{}

		allowed := make(map[string]struct{}, len(worker.AllowedKinds))
		for _, kind := range worker.AllowedKinds {
			if _, valid := validKinds[kind]; !valid {
				return fmt.Errorf("%w: worker %q has unknown kind %q", ErrInvalidTeamWorkerKinds, worker.WorkerAgentID, kind)
			}
			if _, duplicate := allowed[kind]; duplicate {
				return fmt.Errorf("%w: worker %q repeats kind %q", ErrInvalidTeamWorkerKinds, worker.WorkerAgentID, kind)
			}
			allowed[kind] = struct{}{}
		}
		if len(allowed) == 0 {
			return fmt.Errorf("%w: worker %q has no allowed kinds", ErrInvalidTeamWorkerKinds, worker.WorkerAgentID)
		}
		if _, ok := allowed[worker.DefaultKind]; !ok {
			return fmt.Errorf("%w: worker %q default %q is not allowed", ErrInvalidTeamWorkerKinds, worker.WorkerAgentID, worker.DefaultKind)
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
		return TeamEvaluationEvaluated
	}
	return evaluation
}
