package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ErrArchivedTeamImmutable prevents roster history from being rewritten after
// a team has been archived.
var ErrArchivedTeamImmutable = errors.New("archived team roster is immutable")

// ErrTeamWorkerIncompatible indicates that an Agent cannot be referenced as a
// versioned team worker because it contains nested orchestration.
var ErrTeamWorkerIncompatible = errors.New("team worker agent is incompatible")

// TeamRosterWorkerInput is the complete mutable TeamWorker configuration used
// by the atomic roster writer.
type TeamRosterWorkerInput struct {
	WorkerAgentID      string   `json:"worker_agent_id"`
	Duty               string   `json:"duty"`
	WhenToUse          string   `json:"when_to_use"`
	ContextInstruction string   `json:"context_instruction"`
	AllowedKinds       []string `json:"allowed_kinds"`
	DefaultKind        string   `json:"default_kind"`
	ResultRequirement  string   `json:"result_requirement"`
	Enabled            bool     `json:"enabled"`
}

func validateTeamRosterWorkerInputs(workers []TeamRosterWorkerInput) error {
	seen := make(map[string]struct{}, len(workers))
	for index, worker := range workers {
		if worker.WorkerAgentID == "" {
			return fmt.Errorf("worker agent ID is required at index %d", index)
		}
		if _, duplicate := seen[worker.WorkerAgentID]; duplicate {
			return fmt.Errorf("agent %q appears more than once in roster", worker.WorkerAgentID)
		}
		seen[worker.WorkerAgentID] = struct{}{}
		if err := validateTeamWorkerKinds(worker.AllowedKinds, worker.DefaultKind); err != nil {
			return fmt.Errorf("worker %q: %w", worker.WorkerAgentID, err)
		}
	}
	return nil
}

// ValidateTeamWorkerAgentRecord enforces the R6 boundary that a team worker's
// own graph cannot contain cross-agent orchestration.
func ValidateTeamWorkerAgentRecord(rec *AgentRecord) error {
	if rec == nil {
		return fmt.Errorf("%w: missing agent record", ErrTeamWorkerIncompatible)
	}
	if len(rec.SubAgents) > 0 {
		return fmt.Errorf("%w: agent %q defines sub_agents", ErrTeamWorkerIncompatible, rec.Name)
	}
	if rec.GraphDefinition != nil {
		for _, step := range rec.GraphDefinition.Steps {
			if step.Type == "worker" {
				return fmt.Errorf("%w: agent %q graph contains worker step %q", ErrTeamWorkerIncompatible, rec.Name, step.Name)
			}
		}
	}
	return nil
}

// Deprecated: UpdateTeamRoster has no production caller after the public route
// moved to ApplyTeamRosterCommand. It retains legacy removal only for existing
// tests and will be deleted by the follow-up cleanup ticket.
func (r *AgentRegistry) UpdateTeamRoster(
	ctx context.Context,
	workspaceID, teamID, leadAgentID string,
	workerInputs []TeamRosterWorkerInput,
) error {
	workers := cloneTeamRosterWorkers(workerInputs)
	for index := range workers {
		sortKinds(workers[index].AllowedKinds)
	}
	sort.Slice(workers, func(i, j int) bool {
		return workers[i].WorkerAgentID < workers[j].WorkerAgentID
	})
	if err := validateTeamRosterWorkerInputs(workers); err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	team, err := lockRosterTeamForMutation(ctx, tx, workspaceID, teamID)
	if errors.Is(err, ErrTeamWorkerNotFound) {
		return fmt.Errorf("team %q not found in workspace %q", teamID, workspaceID)
	}
	if err != nil {
		return fmt.Errorf("lock team %q: %w", teamID, err)
	}
	if team.Status == "archived" {
		return fmt.Errorf("%w: team %q", ErrArchivedTeamImmutable, teamID)
	}
	managerID, err := findLegacyRosterManager(ctx, tx, workspaceID, teamID, team.LeadAvatarID)
	if err != nil {
		return err
	}
	if err := lockAndValidateRosterAgents(ctx, tx, workspaceID, leadAgentID, workers, managerID); err != nil {
		return err
	}
	current, err := readLockedRosterWorkers(ctx, tx, workspaceID, teamID)
	if err != nil {
		return err
	}
	outcome, err := applyTeamRosterMutationTx(ctx, tx, teamRosterMutationRequest{
		WorkspaceID: workspaceID,
		TeamID:      teamID,
		Team:        team,
		LeadAgentID: leadAgentID,
		Status:      "active",
		Workers:     workers,
		Current:     current,
		ManagerID:   managerID,
		AllowRemove: true,
	})
	if err != nil {
		return err
	}
	if err := verifyTeamRosterMutation(ctx, tx, workspaceID, teamID, outcome); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit team roster: %w", err)
	}
	return nil
}
